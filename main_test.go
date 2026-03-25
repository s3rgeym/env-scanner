package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseEnvFile(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  map[string]string
	}{
		{
			name:  "simple unquoted",
			input: "FOO=Bar",
			want:  map[string]string{"FOO": "Bar"},
		},
		{
			name:  "double quoted",
			input: `FOO="Bar"`,
			want:  map[string]string{"FOO": "Bar"},
		},
		{
			name:  "single quoted",
			input: "FOO='Bar'",
			want:  map[string]string{"FOO": "Bar"},
		},
		{
			name:  "quoted and unquoted same value",
			input: "FOO=Bar\nBAR=\"Bar\"",
			want:  map[string]string{"FOO": "Bar", "BAR": "Bar"},
		},
		{
			name:  "export prefix",
			input: "export FOO=Bar",
			want:  map[string]string{"FOO": "Bar"},
		},
		{
			name:  "export with quotes",
			input: `export FOO="Bar"`,
			want:  map[string]string{"FOO": "Bar"},
		},
		{
			name:  "skip comments",
			input: "# this is a comment\nFOO=Bar",
			want:  map[string]string{"FOO": "Bar"},
		},
		{
			name:  "skip empty lines",
			input: "\n\nFOO=Bar\n\n",
			want:  map[string]string{"FOO": "Bar"},
		},
		{
			name:  "inline comment stripped for unquoted",
			input: "FOO=Bar # inline comment",
			want:  map[string]string{"FOO": "Bar"},
		},
		{
			name:  "inline comment preserved inside quotes",
			input: `FOO="Bar # not a comment"`,
			want:  map[string]string{"FOO": "Bar # not a comment"},
		},
		{
			name:  "value with equals sign",
			input: "FOO=a=b=c",
			want:  map[string]string{"FOO": "a=b=c"},
		},
		{
			name:  "empty value",
			input: "FOO=",
			want:  map[string]string{"FOO": ""},
		},
		{
			name:  "empty quoted value",
			input: `FOO=""`,
			want:  map[string]string{"FOO": ""},
		},
		{
			name:  "whitespace trimmed around key and value",
			input: "  FOO  =  Bar  ",
			want:  map[string]string{"FOO": "Bar"},
		},
		{
			name: "multiple vars",
			input: `DB_HOST=localhost
DB_PORT=5432
DB_PASSWORD="secret123"
API_KEY='abc123def456'
export DEBUG=true`,
			want: map[string]string{
				"DB_HOST":     "localhost",
				"DB_PORT":     "5432",
				"DB_PASSWORD": "secret123",
				"API_KEY":     "abc123def456",
				"DEBUG":       "true",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewParser(&Logger{level: LERROR})
			got := p.ParseEnv(tt.input)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("\ninput: %q\ngot:  %v\nwant: %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestRandomUserAgent(t *testing.T) {
	for i := 0; i < 200; i++ {
		ua := randomUserAgent()
		if strings.Contains(ua, "%!(EXTRA") {
			t.Errorf("extra args leaked into UA: %s", ua)
		}
		if strings.Contains(ua, "%d") {
			t.Errorf("unformatted placeholder in UA: %s", ua)
		}
		if ua == "" {
			t.Error("empty UA generated")
		}
	}
}

func TestValidateConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "valid",
			cfg: Config{
				ConnectTimeout: time.Second,
				RequestTimeout: 2 * time.Second,
				Workers:        1,
				RateLimit:      0,
			},
		},
		{
			name: "invalid workers",
			cfg: Config{
				ConnectTimeout: time.Second,
				RequestTimeout: 2 * time.Second,
				Workers:        0,
			},
			want: "workers must be greater than 0",
		},
		{
			name: "invalid connect timeout",
			cfg: Config{
				ConnectTimeout: 0,
				RequestTimeout: 2 * time.Second,
				Workers:        1,
			},
			want: "connect-timeout must be greater than 0",
		},
		{
			name: "invalid request timeout",
			cfg: Config{
				ConnectTimeout: time.Second,
				RequestTimeout: 0,
				Workers:        1,
			},
			want: "request-timeout must be greater than 0",
		},
		{
			name: "invalid rate limit",
			cfg: Config{
				ConnectTimeout: time.Second,
				RequestTimeout: 2 * time.Second,
				Workers:        1,
				RateLimit:      -1,
			},
			want: "rate-limit must be non-negative",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateConfig(&tt.cfg)
			if tt.want == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.want != "" {
				if err == nil {
					t.Fatalf("expected error containing %q", tt.want)
				}
				if !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("got error %q, want substring %q", err.Error(), tt.want)
				}
			}
		})
	}
}

func TestWorkerProcessFailsOnBodyReadError(t *testing.T) {
	cfg := &Config{
		ConnectTimeout: time.Second,
		RequestTimeout: time.Second,
		Workers:        1,
	}
	worker := NewWorker(
		1,
		cfg,
		&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       &errorReadCloser{err: errors.New("read failed")},
				Header:     make(http.Header),
			}, nil
		})},
		nil,
		NewParser(&Logger{level: LERROR}),
		&Logger{level: LERROR},
	)

	_, err := worker.process(context.Background(), Task{
		FullURL: "http://example.com/.env",
		Path:    ".env",
	})
	if err == nil {
		t.Fatal("expected process to fail on body read error")
	}
	if !strings.Contains(err.Error(), "read failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestScannerRunReturnsWriterError(t *testing.T) {
	cfg := &Config{
		ConnectTimeout: time.Second,
		RequestTimeout: time.Second,
		Workers:        2,
		OutputFile:     filepath.Join(t.TempDir(), "missing", "out.json"),
	}
	scanner := NewScanner(cfg, &Logger{level: LERROR})
	scanner.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := ""
		if strings.HasSuffix(req.URL.Path, ".env") {
			body = "FOO=bar\n"
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})}

	err := scanner.Run(context.Background(), []string{"http://example.com"})
	if err == nil {
		t.Fatal("expected scanner to return writer error")
	}
	if !strings.Contains(err.Error(), "no such file or directory") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGenerateTasksIncludesExtendedEnvPaths(t *testing.T) {
	cfg := &Config{
		ConnectTimeout: time.Second,
		RequestTimeout: time.Second,
		Workers:        1,
	}
	scanner := NewScanner(cfg, &Logger{level: LERROR})
	tasks := make(chan Task, len(scanPaths))

	scanner.generateTasks(context.Background(), []string{"http://example.com"}, tasks)

	got := make(map[string]string, len(scanPaths))
	for i := 0; i < len(scanPaths); i++ {
		task := <-tasks
		got[task.Path] = task.FullURL
	}

	wantPaths := []string{
		".env",
		".env.local",
		"prod.env",
		"dev.env",
		"test.env",
		"config/.env",
		"docker/.env",
		"phpinfo.php",
		"info.php",
	}

	if len(got) != len(wantPaths) {
		t.Fatalf("got %d tasks, want %d", len(got), len(wantPaths))
	}

	for _, path := range wantPaths {
		fullURL, ok := got[path]
		if !ok {
			t.Fatalf("missing generated task for path %q", path)
		}
		if !strings.HasSuffix(fullURL, "/"+path) {
			t.Fatalf("generated URL %q does not end with /%s", fullURL, path)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type errorReadCloser struct {
	err error
}

func (r *errorReadCloser) Read([]byte) (int, error) {
	return 0, r.err
}

func (r *errorReadCloser) Close() error {
	return nil
}
