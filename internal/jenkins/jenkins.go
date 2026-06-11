// Package jenkins fetches and parses test-failure data from a Jenkins build
// using the JSON API. Given a build (or test-report) URL, it appends
// /api/json, authenticates with HTTP Basic auth (user + API token), and
// extracts the set of failed test cases.
package jenkins

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// FailedTest is a single failing test case, with the full output captured from
// the Jenkins test report. ErrorStackTrace/Stdout/Stderr are the "output of the
// test failure" that the LLM diagnoses.
type FailedTest struct {
	ClassName       string
	Name            string
	Status          string // FAILED, REGRESSION, ...
	Duration        float64
	ErrorDetails    string
	ErrorStackTrace string
	Stdout          string
	Stderr          string
	ReportURL       string // best-effort human/API URL to this case's report
}

// FullName returns a stable "class.method" identifier for the test.
func (t FailedTest) FullName() string {
	if t.ClassName == "" {
		return t.Name
	}
	return t.ClassName + "." + t.Name
}

// Client talks to a Jenkins instance.
type Client struct {
	http     *http.Client
	user     string
	apiToken string
}

// NewClient builds a Jenkins client. user/apiToken are used for HTTP Basic
// auth; both may be empty for an unauthenticated (e.g. anonymous) instance.
func NewClient(user, apiToken string) *Client {
	return &Client{
		http:     &http.Client{Timeout: 30 * time.Second},
		user:     user,
		apiToken: apiToken,
	}
}

// jsonAPIURL normalizes an arbitrary Jenkins URL to its JSON API endpoint by
// stripping any trailing slash, an existing /api/json suffix, and the query
// string. If the URL doesn't already point at a test report it appends
// /testReport, then it appends /api/json with a depth that pulls in case
// details. So a bare build URL (…/1234/) and a test-report URL
// (…/1234/testReport/) both resolve to …/1234/testReport/api/json.
func jsonAPIURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("URL %q must be absolute (scheme + host)", raw)
	}
	p := strings.TrimRight(u.Path, "/")
	p = strings.TrimSuffix(p, "/api/json")
	p = strings.TrimRight(p, "/")
	if !strings.HasSuffix(p, "/testReport") {
		p += "/testReport"
	}
	u.Path = p + "/api/json"
	// depth=1 ensures suites[].cases[] (with errorStackTrace/stdout) are inlined.
	q := url.Values{}
	q.Set("depth", "1")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// testReport mirrors the subset of Jenkins' testReport/api/json we care about.
type testReport struct {
	Suites []struct {
		Name  string `json:"name"`
		Cases []struct {
			ClassName       string  `json:"className"`
			Name            string  `json:"name"`
			Status          string  `json:"status"`
			Duration        float64 `json:"duration"`
			ErrorDetails    string  `json:"errorDetails"`
			ErrorStackTrace string  `json:"errorStackTrace"`
			Stdout          string  `json:"stdout"`
			Stderr          string  `json:"stderr"`
		} `json:"cases"`
	} `json:"suites"`
}

// FetchFailedTests retrieves the build/test-report JSON for buildURL and
// returns every failing case. buildURL may be the build URL or the testReport
// URL; /api/json is appended automatically.
func (c *Client) FetchFailedTests(ctx context.Context, buildURL string) ([]FailedTest, error) {
	apiURL, err := jsonAPIURL(buildURL)
	if err != nil {
		return nil, err
	}

	body, err := c.get(ctx, apiURL)
	if err != nil {
		return nil, err
	}

	var report testReport
	if err := json.Unmarshal(body, &report); err != nil {
		return nil, fmt.Errorf("decoding test report from %s: %w", apiURL, err)
	}

	// reportBase is the build URL with any /api/json or /testReport suffix
	// stripped, so caseReportURL can re-append testReport/<case> without doubling.
	reportBase := strings.TrimRight(buildURL, "/")
	reportBase = strings.TrimSuffix(reportBase, "/api/json")
	reportBase = strings.TrimRight(reportBase, "/")
	reportBase = strings.TrimSuffix(reportBase, "/testReport")

	var failures []FailedTest
	for _, suite := range report.Suites {
		for _, tc := range suite.Cases {
			if !isFailure(tc.Status) {
				continue
			}
			failures = append(failures, FailedTest{
				ClassName:       tc.ClassName,
				Name:            tc.Name,
				Status:          tc.Status,
				Duration:        tc.Duration,
				ErrorDetails:    tc.ErrorDetails,
				ErrorStackTrace: tc.ErrorStackTrace,
				Stdout:          tc.Stdout,
				Stderr:          tc.Stderr,
				ReportURL:       caseReportURL(reportBase, tc.ClassName, tc.Name),
			})
		}
	}
	return failures, nil
}

func isFailure(status string) bool {
	switch strings.ToUpper(status) {
	case "FAILED", "REGRESSION", "ERROR":
		return true
	default:
		return false
	}
}

func (c *Client) get(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if c.user != "" || c.apiToken != "" {
		req.SetBasicAuth(c.user, c.apiToken)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", u, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("GET %s: %s (check jenkins.user / api_token)", u, resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: unexpected status %s", u, resp.Status)
	}

	const maxBody = 64 << 20 // 64 MiB guard for very large reports
	return io.ReadAll(io.LimitReader(resp.Body, maxBody))
}

// caseReportURL constructs Jenkins' per-case report URL. Jenkins mangles
// non-alphanumeric characters to underscores in the path segment; this mirrors
// that well enough for a clickable link, though it is best-effort.
func caseReportURL(base, className, name string) string {
	if base == "" {
		return ""
	}
	pkg, cls := splitClass(className)
	seg := func(s string) string { return nonAlnum.ReplaceAllString(s, "_") }
	parts := []string{strings.TrimRight(base, "/"), "testReport"}
	if pkg != "" {
		parts = append(parts, seg(pkg))
	}
	if cls != "" {
		parts = append(parts, seg(cls))
	}
	parts = append(parts, seg(name))
	return strings.Join(parts, "/") + "/"
}

var nonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]`)

func splitClass(fqcn string) (pkg, cls string) {
	i := strings.LastIndex(fqcn, ".")
	if i < 0 {
		return "", fqcn
	}
	return fqcn[:i], fqcn[i+1:]
}
