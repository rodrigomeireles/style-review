package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/actions-go/toolkit/core"
)

var pageIDPattern = regexp.MustCompile(`(?i)([0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)
var blockIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func pageID(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("Invalid Notion URL")
	}
	host := strings.ToLower(u.Hostname())
	if u.Scheme != "https" || u.Port() != "" || u.User != nil || !(host == "notion.so" || host == "www.notion.so" || host == "notion.site" || strings.HasSuffix(host, ".notion.site")) {
		return "", errors.New("Unsupported Notion URL")
	}
	m := pageIDPattern.FindStringSubmatch(strings.TrimSuffix(u.Path, "/"))
	if m == nil {
		return "", errors.New("Missing Notion page ID")
	}
	return strings.ToLower(strings.ReplaceAll(m[1], "-", "")), nil
}

type citation struct {
	Source  string `json:"source"`
	BlockID string `json:"block_id"`
	Excerpt string `json:"excerpt"`
	URL     string `json:"url"`
}

func validCitation(key string, c citation) bool {
	if c.Source == "STYLE" {
		expected, _, err := styleRules()
		return err == nil && expected[key] == c && strings.TrimSpace(c.Excerpt) != ""
	}
	u, err := url.Parse(c.URL)
	if err != nil {
		return false
	}
	id, err := pageID(c.URL)
	return err == nil && u.Host == "www.notion.so" && u.Path == "/"+id && u.RawQuery == "" && u.Fragment == c.BlockID && blockIDPattern.MatchString(c.BlockID) && key == c.Source+":"+c.BlockID && strings.TrimSpace(c.Excerpt) != ""
}

type notionClient struct {
	http        *http.Client
	base, token string
}

func (c notionClient) get(ctx context.Context, path string, out any) error {
	for attempt := 0; attempt < 4; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Notion-Version", "2026-03-11")
		req.Header.Set("Accept", "application/json")
		res, err := c.http.Do(req)
		if err != nil {
			return errors.New("Notion request failed")
		}
		if (res.StatusCode == 429 || res.StatusCode >= 500) && attempt < 3 {
			delay := time.Duration(attempt+1) * 800 * time.Millisecond
			if n, e := strconv.Atoi(res.Header.Get("Retry-After")); e == nil && n > 0 {
				delay = time.Duration(min(n, 10)) * time.Second
			}
			res.Body.Close()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
			continue
		}
		if res.StatusCode != 200 {
			res.Body.Close()
			return fmt.Errorf("Notion fetch failed: HTTP %d", res.StatusCode)
		}
		b, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			return errors.New("Notion response read failed")
		}
		if json.Unmarshal(b, out) != nil {
			return errors.New("Invalid Notion response JSON")
		}
		return nil
	}
	return errors.New("Notion fetch retries exhausted")
}
func (c notionClient) markdown(ctx context.Context, id string) (string, error) {
	var b struct {
		Object    string   `json:"object"`
		Markdown  string   `json:"markdown"`
		Truncated bool     `json:"truncated"`
		Unknown   []string `json:"unknown_block_ids"`
	}
	if err := c.get(ctx, "/pages/"+id+"/markdown?markdown_version=v2", &b); err != nil {
		return "", err
	}
	if b.Truncated || len(b.Unknown) > 0 {
		return "", errors.New("Notion API returned incomplete Markdown (truncated or unknown blocks); raising local limits cannot recover missing content")
	}
	if b.Object != "page_markdown" || strings.TrimSpace(b.Markdown) == "" {
		return "", errors.New("Empty or unexpected Notion Markdown")
	}
	return b.Markdown, nil
}
func (c notionClient) citations(ctx context.Context, page, source string) (map[string]citation, error) {
	result := map[string]citation{}
	visited := map[string]bool{}
	var visit func(string, int) error
	visit = func(parent string, depth int) error {
		if depth > 100 {
			return errors.New("Notion block nesting limit exceeded")
		}
		cursor := ""
		cursors := map[string]bool{}
		for {
			q := url.Values{"page_size": {"100"}}
			if cursor != "" {
				q.Set("start_cursor", cursor)
			}
			var b struct {
				Object  string                       `json:"object"`
				Blocks  []map[string]json.RawMessage `json:"results"`
				HasMore *bool                        `json:"has_more"`
				Next    string                       `json:"next_cursor"`
			}
			if err := c.get(ctx, "/blocks/"+parent+"/children?"+q.Encode(), &b); err != nil {
				return err
			}
			if b.Object != "list" || b.Blocks == nil || b.HasMore == nil {
				return errors.New("Unexpected Notion blocks response")
			}
			for _, raw := range b.Blocks {
				var id, typ string
				var children bool
				if json.Unmarshal(raw["id"], &id) != nil || json.Unmarshal(raw["type"], &typ) != nil || json.Unmarshal(raw["has_children"], &children) != nil {
					return errors.New("Invalid Notion block")
				}
				id = strings.ToLower(strings.ReplaceAll(id, "-", ""))
				if !blockIDPattern.MatchString(id) || visited[id] {
					return errors.New("Invalid or repeated Notion block")
				}
				visited[id] = true
				if len(visited) > 10000 {
					return errors.New("Notion block count limit exceeded")
				}
				var data struct {
					Rich []struct {
						Plain *string `json:"plain_text"`
						Type  string  `json:"type"`
						Text  struct {
							Content *string `json:"content"`
						} `json:"text"`
					} `json:"rich_text"`
				}
				if json.Unmarshal(raw[typ], &data) != nil {
					return errors.New("Invalid Notion rich text")
				}
				excerpt := ""
				for _, part := range data.Rich {
					if part.Plain != nil {
						excerpt += *part.Plain
					} else if part.Type == "text" && part.Text.Content != nil {
						excerpt += *part.Text.Content
					} else {
						return errors.New("Missing Notion rich text plain text")
					}
				}
				if strings.TrimSpace(excerpt) != "" {
					entry := citation{source, id, excerpt, "https://www.notion.so/" + page + "#" + id}
					result[source+":"+id] = entry
				}
				if children {
					if err := visit(id, depth+1); err != nil {
						return err
					}
				}
			}
			if !*b.HasMore {
				break
			}
			cursor = b.Next
			if cursor == "" || cursors[cursor] {
				return errors.New("Invalid Notion pagination cursor")
			}
			cursors[cursor] = true
		}
		return nil
	}
	if err := visit(page, 0); err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return nil, errors.New("No citable Notion rule blocks")
	}
	return result, nil
}
func loadNotion(ctx context.Context) error {
	styleCitations, styleText, err := styleRules()
	if err != nil {
		return err
	}
	token := os.Getenv("NOTION_API_KEY")
	var entries []string
	for _, line := range strings.Split(os.Getenv("NOTION_STYLE_URLS"), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			entries = append(entries, s)
		}
	}
	if len(entries) == 0 && styleText == "" {
		return errors.New("No style rules: add root STYLE.md or configure NOTION_STYLE_URLS and NOTION_API_KEY")
	}
	if len(entries) > 0 && token == "" {
		return errors.New("NOTION_API_KEY secret must be set for configured Notion pages")
	}
	c := notionClient{&http.Client{Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, "https://api.notion.com/v1", token}
	seen := map[string]bool{}
	sources := map[string]string{}
	citations := styleCitations
	var blocks []string
	if styleText != "" {
		sources["STYLE"] = "STYLE.md"
		encoded, _ := json.Marshal(styleCitations)
		blocks = append(blocks, "### STYLE — STYLE.md (authoritative; overrides conflicting Notion rules)\n\n"+styleText+"\n\n#### Verified source citations (JSON)\n"+string(encoded))
	}
	for _, entry := range entries {
		id, err := pageID(entry)
		if err != nil {
			return err
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		source := fmt.Sprintf("N%d", len(seen))
		markdown, err := c.markdown(ctx, id)
		if err != nil {
			return err
		}
		cs, err := c.citations(ctx, id, source)
		if err != nil {
			return err
		}
		for k, v := range cs {
			citations[k] = v
		}
		sources[source] = entry
		encoded, _ := json.Marshal(cs)
		blocks = append(blocks, fmt.Sprintf("### %s — %s\n\n%s\n\n#### Verified block citations (JSON)\n%s", source, entry, markdown, encoded))
	}
	text := strings.Join(blocks, "\n\n---\n\n")
	hash := sha256.Sum256([]byte(text))
	revision := fmt.Sprintf("%x", hash)
	contextFile, err := writeRulesFile("style-rules-*.md", []byte(text))
	if err != nil {
		return err
	}
	metadata, err := json.Marshal(ruleMetadata{Sources: sources, Citations: citations})
	if err != nil {
		os.Remove(contextFile)
		return err
	}
	metadataFile, err := writeRulesFile("style-citations-*.json", metadata)
	if err != nil {
		os.Remove(contextFile)
		return err
	}
	core.SetOutput("context_file", contextFile)
	core.SetOutput("metadata_file", metadataFile)
	core.SetOutput("revision", revision)
	core.Infof("Loaded %d style source(s), SHA-256 %s", len(sources), revision[:12])
	return nil
}

func styleRules() (map[string]citation, string, error) {
	result := map[string]citation{}
	b, err := os.ReadFile("STYLE.md")
	if os.IsNotExist(err) {
		return result, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(string(b)) == "" {
		return nil, "", errors.New("STYLE.md is empty")
	}
	shaBytes, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return nil, "", errors.New("STYLE.md requires a Git checkout")
	}
	sha := strings.TrimSpace(string(shaBytes))
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(sha) {
		return nil, "", errors.New("Invalid checkout SHA")
	}
	committed, err := exec.Command("git", "show", sha+":STYLE.md").Output()
	if err != nil || !bytes.Equal(bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")), bytes.ReplaceAll(committed, []byte("\r\n"), []byte("\n"))) {
		return nil, "", errors.New("STYLE.md must match the immutable checkout revision")
	}
	repo := os.Getenv("GITHUB_REPOSITORY")
	if repo == "" {
		remote, e := exec.Command("git", "remote", "get-url", "origin").Output()
		if e == nil {
			repo = strings.TrimSuffix(strings.TrimSpace(string(remote)), ".git")
			repo = strings.TrimPrefix(strings.TrimPrefix(repo, "https://github.com/"), "git@github.com:")
		}
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(repo) {
		return nil, "", errors.New("STYLE.md requires GITHUB_REPOSITORY (owner/repo) or a GitHub origin remote")
	}
	server := strings.TrimRight(os.Getenv("GITHUB_SERVER_URL"), "/")
	if server == "" {
		server = "https://github.com"
	}
	u, err := url.Parse(server)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return nil, "", errors.New("Invalid GITHUB_SERVER_URL")
	}
	for i, line := range strings.Split(string(committed), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		anchor := fmt.Sprintf("L%d", i+1)
		result["STYLE:"+anchor] = citation{"STYLE", anchor, line, server + "/" + repo + "/blob/" + sha + "/STYLE.md#" + anchor}
	}
	return result, string(committed), nil
}

// Files survive between workflow steps; only their absolute paths cross Actions outputs.
type ruleMetadata struct {
	Sources   map[string]string   `json:"sources"`
	Citations map[string]citation `json:"citations"`
}

func writeRulesFile(pattern string, data []byte) (string, error) {
	f, err := os.CreateTemp(os.Getenv("RUNNER_TEMP"), pattern)
	if err != nil {
		return "", err
	}
	name, err := filepath.Abs(f.Name())
	if err == nil {
		_, err = f.Write(data)
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return name, nil
}

func readRuleMetadata(path string) (ruleMetadata, error) {
	var metadata ruleMetadata
	data, err := os.ReadFile(path)
	if err != nil {
		return metadata, err
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return metadata, err
	}
	if len(metadata.Sources) == 0 || len(metadata.Citations) == 0 {
		return metadata, errors.New("Missing verified rule sources or citations")
	}
	return metadata, nil
}
