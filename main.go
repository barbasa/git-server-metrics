package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gopkg.in/yaml.v3"
)

var validRepoNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+([a-zA-Z0-9_-]+)*$`)

type RepoSpec struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"`
}

func (r *RepoSpec) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		var p string
		if err := value.Decode(&p); err != nil {
			return err
		}
		r.Path = p
		r.Name = defaultRepoNameFromPath(p)
		return nil
	case yaml.MappingNode:
		type raw RepoSpec
		var dec raw
		if err := value.Decode(&dec); err != nil {
			return err
		}
		r.Path = dec.Path
		r.Name = dec.Name
		if r.Name == "" {
			r.Name = defaultRepoNameFromPath(r.Path)
		}
		return nil
	default:
		return fmt.Errorf("invalid repo entry, expected string or map")
	}
}

type Config struct {
	ListenAddr     string        `yaml:"listen_addr"`
	GitBin         string        `yaml:"git_bin"`
	PoolSize       int           `yaml:"pool_size"`
	CommandTimeout time.Duration `yaml:"command_timeout"`
	Repos          []RepoSpec    `yaml:"repos"`
	ScrapeInterval time.Duration `yaml:"scrape_interval"`
}

func (c *Config) normalize() error {
	if c.ListenAddr == "" {
		c.ListenAddr = ":9108"
	}
	if c.GitBin == "" {
		c.GitBin = "git"
	}
	if c.PoolSize <= 0 {
		c.PoolSize = 1
	}
	if c.CommandTimeout <= 0 {
		c.CommandTimeout = 20 * time.Second
	}
	if c.ScrapeInterval <= 0 {
		c.ScrapeInterval = 30 * time.Second
	}
	if len(c.Repos) == 0 {
		return errors.New("no repositories configured")
	}
	for i, repo := range c.Repos {
		if strings.TrimSpace(repo.Path) == "" {
			return fmt.Errorf("repos[%d] has empty path", i)
		}
		if strings.TrimSpace(repo.Name) == "" {
			c.Repos[i].Name = defaultRepoNameFromPath(repo.Path)
		}
	}
	return nil
}

func loadConfig(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()

	var cfg Config
	if err := yaml.NewDecoder(f).Decode(&cfg); err != nil {
		return Config{}, err
	}
	if err := cfg.normalize(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

var (
	repoLabels = []string{"repo_name"}

	mNumberOfBitmaps = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "plugins_git_repo_metrics_numberofbitmaps",
		Help: "Number of bitmap indexes in objects/pack.",
	}, repoLabels)
	mNumberOfLooseObjects = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "plugins_git_repo_metrics_numberoflooseobjects",
		Help: "Number of loose objects.",
	}, repoLabels)
	mNumberOfLooseRefs = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "plugins_git_repo_metrics_numberoflooserefs",
		Help: "Number of loose refs.",
	}, repoLabels)
	mNumberOfPackedObjects = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "plugins_git_repo_metrics_numberofpackedobjects",
		Help: "Number of packed objects.",
	}, repoLabels)
	mNumberOfPackedRefs = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "plugins_git_repo_metrics_numberofpackedrefs",
		Help: "Number of packed refs.",
	}, repoLabels)
	mNumberOfPackFiles = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "plugins_git_repo_metrics_numberofpackfiles",
		Help: "Number of pack files.",
	}, repoLabels)
	mSizeOfLooseObjects = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "plugins_git_repo_metrics_sizeoflooseobjects",
		Help: "Loose objects size in KiB as reported by git count-objects.",
	}, repoLabels)
	mSizeOfPackedObjects = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "plugins_git_repo_metrics_sizeofpackedobjects",
		Help: "Packed objects size in KiB as reported by git count-objects.",
	}, repoLabels)
	mGitMetricsCollectionTime = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "plugins_git_repo_metrics_gitmetricscollectiontime",
		Help: "UNIX time in milliseconds when git metrics were collected.",
	}, repoLabels)

	mNumberOfKeepFiles = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "plugins_git_repo_metrics_numberofkeepfiles",
		Help: "Number of .keep files in objects/.",
	}, repoLabels)
	mNumberOfFiles = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "plugins_git_repo_metrics_numberoffiles",
		Help: "Number of files under objects/.",
	}, repoLabels)
	mNumberOfDirectories = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "plugins_git_repo_metrics_numberofdirectories",
		Help: "Number of directories under objects/ (including objects/ itself).",
	}, repoLabels)
	mNumberOfEmptyDirectories = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "plugins_git_repo_metrics_numberofemptydirectories",
		Help: "Number of empty directories under objects/.",
	}, repoLabels)
	mFSMetricsCollectionTime = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "plugins_git_repo_metrics_fsmetricscollectiontime",
		Help: "UNIX time in milliseconds when filesystem metrics were collected.",
	}, repoLabels)
)

type GitStats struct {
	NumberOfPackedObjects float64
	NumberOfPackFiles     float64
	NumberOfLooseObjects  float64
	NumberOfLooseRefs     float64
	NumberOfPackedRefs    float64
	SizeOfLooseObjects    float64
	SizeOfPackedObjects   float64
	NumberOfBitmaps       float64
}

type FSStats struct {
	NumberOfKeepFiles        float64
	NumberOfEmptyDirectories float64
	NumberOfDirectories      float64
	NumberOfFiles            float64
}

type Runner struct {
	cfg Config
}

func (r Runner) runGitCommand(ctx context.Context, repoPath string, args ...string) ([]byte, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, r.cfg.CommandTimeout)
	defer cancel()

	baseArgs := []string{"-C", repoPath}
	baseArgs = append(baseArgs, args...)
	cmd := exec.CommandContext(cmdCtx, r.cfg.GitBin, baseArgs...)
	out, err := cmd.Output()
	if err != nil {
		if ee := new(exec.ExitError); errors.As(err, &ee) {
			return nil, fmt.Errorf("git %s failed: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("git %s failed: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

func (r Runner) collectGitStats(ctx context.Context, repo RepoSpec) (GitStats, error) {
	out, err := r.runGitCommand(ctx, repo.Path, "count-objects", "-v")
	if err != nil {
		return GitStats{}, err
	}

	parsed, err := parseCountObjectsOutput(string(out))
	if err != nil {
		return GitStats{}, err
	}

	looseRefs, err := countLooseRefs(repo.Path)
	if err != nil {
		return GitStats{}, err
	}
	packedRefs, err := countPackedRefs(repo.Path)
	if err != nil {
		return GitStats{}, err
	}
	bitmaps, err := countBitmapFiles(repo.Path)
	if err != nil {
		return GitStats{}, err
	}

	parsed.NumberOfLooseRefs = float64(looseRefs)
	parsed.NumberOfPackedRefs = float64(packedRefs)
	parsed.NumberOfBitmaps = float64(bitmaps)
	return parsed, nil
}

func parseCountObjectsOutput(out string) (GitStats, error) {
	vals := map[string]float64{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		k := strings.TrimSpace(parts[0])
		v := strings.TrimSpace(parts[1])
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			continue
		}
		vals[k] = f
	}
	if err := sc.Err(); err != nil {
		return GitStats{}, err
	}
	return GitStats{
		NumberOfLooseObjects:  vals["count"],
		SizeOfLooseObjects:    vals["size"],
		NumberOfPackedObjects: vals["in-pack"],
		NumberOfPackFiles:     vals["packs"],
		SizeOfPackedObjects:   vals["size-pack"],
	}, nil
}

func countLooseRefs(repoPath string) (int, error) {
	refsDir := filepath.Join(repoPath, "refs")
	stack := []string{refsDir}
	total := 0

	for len(stack) > 0 {
		dir := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		entries, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return 0, err
		}
		for _, ent := range entries {
			if ent.IsDir() {
				stack = append(stack, filepath.Join(dir, ent.Name()))
				continue
			}
			total++
		}
	}
	return total, nil
}

func countPackedRefs(repoPath string) (int, error) {
	packedRefsPath := filepath.Join(repoPath, "packed-refs")
	f, err := os.Open(packedRefsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	total := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		total++
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return total, nil
}

func countBitmapFiles(repoPath string) (int, error) {
	packDir := filepath.Join(repoPath, "objects", "pack")
	entries, err := os.ReadDir(packDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}

	bitmaps := 0

	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".bitmap") {
			continue
		}
		bitmaps++
	}

	return bitmaps, nil
}

func collectFSStats(repo RepoSpec) (FSStats, error) {
	objectsDir := filepath.Join(repo.Path, "objects")
	stack := []string{objectsDir}
	stats := FSStats{}

	for len(stack) > 0 {
		dir := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		entries, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return FSStats{}, err
		}

		stats.NumberOfDirectories++
		if len(entries) == 0 {
			stats.NumberOfEmptyDirectories++
		}

		for _, ent := range entries {
			if ent.IsDir() {
				stack = append(stack, filepath.Join(dir, ent.Name()))
				continue
			}
			stats.NumberOfFiles++
			if strings.HasSuffix(ent.Name(), ".keep") {
				stats.NumberOfKeepFiles++
			}
		}
	}
	return stats, nil
}

func sanitizeRepoName(name string) string {
	if strings.Contains(name, "_0x") {
		name = strings.ReplaceAll(name, "_0x", "_0x_0x")
	}
	if validRepoNamePattern.MatchString(name) {
		return name
	}

	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
			continue
		}
		b.WriteString(fmt.Sprintf("_0x%X_", r))
	}
	return b.String()
}

func defaultRepoNameFromPath(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, ".git")
}

func setGitMetrics(repoName string, stats GitStats, collectionTimeMs int64) {
	labels := prometheus.Labels{"repo_name": repoName}
	mNumberOfBitmaps.With(labels).Set(stats.NumberOfBitmaps)
	mNumberOfLooseObjects.With(labels).Set(stats.NumberOfLooseObjects)
	mNumberOfLooseRefs.With(labels).Set(stats.NumberOfLooseRefs)
	mNumberOfPackedObjects.With(labels).Set(stats.NumberOfPackedObjects)
	mNumberOfPackedRefs.With(labels).Set(stats.NumberOfPackedRefs)
	mNumberOfPackFiles.With(labels).Set(stats.NumberOfPackFiles)
	mSizeOfLooseObjects.With(labels).Set(stats.SizeOfLooseObjects)
	mSizeOfPackedObjects.With(labels).Set(stats.SizeOfPackedObjects)
	mGitMetricsCollectionTime.With(labels).Set(float64(collectionTimeMs))
}

func setFSMetrics(repoName string, stats FSStats, collectionTimeMs int64) {
	labels := prometheus.Labels{"repo_name": repoName}
	mNumberOfKeepFiles.With(labels).Set(stats.NumberOfKeepFiles)
	mNumberOfFiles.With(labels).Set(stats.NumberOfFiles)
	mNumberOfDirectories.With(labels).Set(stats.NumberOfDirectories)
	mNumberOfEmptyDirectories.With(labels).Set(stats.NumberOfEmptyDirectories)
	mFSMetricsCollectionTime.With(labels).Set(float64(collectionTimeMs))
}

func runWithPool(ctx context.Context, poolSize int, repos []RepoSpec, fn func(context.Context, RepoSpec)) {
	jobs := make(chan RepoSpec)
	var wg sync.WaitGroup

	for i := 0; i < poolSize; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case repo, ok := <-jobs:
					if !ok {
						return
					}
					fn(ctx, repo)
				}
			}
		}()
	}

	for _, repo := range repos {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		case jobs <- repo:
		}
	}
	close(jobs)
	wg.Wait()
}

func runPeriodic(ctx context.Context, interval time.Duration, fn func(context.Context)) {
	for {
		fn(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func scrapeAllMetrics(ctx context.Context, runner Runner, cfg Config) {
	runWithPool(ctx, cfg.PoolSize, cfg.Repos, func(c context.Context, repo RepoSpec) {
		stats, err := runner.collectGitStats(c, repo)
		if err != nil {
			log.Printf("git metrics failed for %s (%s): %v", repo.Name, repo.Path, err)
		} else {
			setGitMetrics(repo.Name, stats, time.Now().UnixMilli())
		}

		fsStats, err := collectFSStats(repo)
		if err != nil {
			log.Printf("fs metrics failed for %s (%s): %v", repo.Name, repo.Path, err)
		} else {
			setFSMetrics(repo.Name, fsStats, time.Now().UnixMilli())
		}
	})
}

func main() {
	cfgPath := "config.yaml"
	if v := os.Getenv("CONFIG"); strings.TrimSpace(v) != "" {
		cfgPath = v
	}
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	prometheus.MustRegister(
		mNumberOfBitmaps,
		mNumberOfLooseObjects,
		mNumberOfLooseRefs,
		mNumberOfPackedObjects,
		mNumberOfPackedRefs,
		mNumberOfPackFiles,
		mSizeOfLooseObjects,
		mSizeOfPackedObjects,
		mGitMetricsCollectionTime,
		mNumberOfKeepFiles,
		mNumberOfFiles,
		mNumberOfDirectories,
		mNumberOfEmptyDirectories,
		mFSMetricsCollectionTime,
	)

	runner := Runner{cfg: cfg}
	for i := range cfg.Repos {
		cfg.Repos[i].Name = sanitizeRepoName(cfg.Repos[i].Name)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go runPeriodic(ctx, cfg.ScrapeInterval, func(runCtx context.Context) {
		scrapeAllMetrics(runCtx, runner, cfg)
	})

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("serving metrics on %s/metrics", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server failed: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("http shutdown failed: %v", err)
	}
}
