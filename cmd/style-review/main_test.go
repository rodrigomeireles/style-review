package main

import (
	"context"
	"encoding/json"

	"fmt"
	gh "github.com/google/go-github/v74/github"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestAddedLines(t *testing.T) {
	got := addedLines("@@ -2,3 +2,4 @@\n unchanged\n-old\n+new\n+extra\n unchanged\n\\ No newline at end of file\n@@ -20 +21 @@\n-old\n+new")
	for _, n := range []int{3, 4, 21} {
		if got[n] == 0 {
			t.Errorf("missing added line %d", n)
		}
	}
	if len(got) != 3 {
		t.Fatalf("unexpected added lines: %v", got)
	}
}

func TestPageID(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	for _, raw := range []string{"https://www.notion.so/Rules-" + id, "https://team.notion.site/01234567-89ab-cdef-0123-456789abcdef?x=1"} {
		got, err := pageID(raw)
		if err != nil || got != id {
			t.Errorf("pageID(%q) = %q, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"http://www.notion.so/" + id, "https://evil.example/" + id, "https://user@www.notion.so/" + id, "https://www.notion.so/no-id"} {
		if _, err := pageID(raw); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
}

func TestExactCitation(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	c := citation{"N1", id, "Preserve [error] context.", "https://www.notion.so/" + id + "#" + id}
	if !validCitation("N1:"+id, c) {
		t.Fatal("valid citation rejected")
	}
	body, marker := renderFinding(finding{Rule: "Errors", Message: "Wrap the error"}, c, "head", "revision")
	if !strings.Contains(body, "[Preserve \\[error\\] context.]("+c.URL+")") || !strings.Contains(body, marker) {
		t.Fatalf("missing exact excerpt hyperlink: %s", body)
	}
	c.URL = "https://www.notion.so/" + id
	if validCitation("N1:"+id, c) {
		t.Fatal("page-only citation accepted")
	}
}

func TestNotionBlocks(t *testing.T) {
	page := "0123456789abcdef0123456789abcdef"
	block := "abcdef0123456789abcdef0123456789"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing authentication")
		}
		if strings.Contains(r.URL.Path, "markdown") {
			fmt.Fprint(w, `{"object":"page_markdown","markdown":"Rules","truncated":false,"unknown_block_ids":[]}`)
		} else {
			fmt.Fprintf(w, `{"object":"list","results":[{"id":%q,"type":"paragraph","has_children":false,"paragraph":{"rich_text":[{"plain_text":"Actual rule"}]}}],"has_more":false,"next_cursor":null}`, block)
		}
	}))
	defer server.Close()
	c := notionClient{server.Client(), server.URL, "secret"}
	if text, err := c.markdown(context.Background(), page); err != nil || text != "Rules" {
		t.Fatalf("markdown: %q %v", text, err)
	}
	citations, err := c.citations(context.Background(), page, "N1")
	if err != nil {
		t.Fatal(err)
	}
	entry := citations["N1:"+block]
	if entry.Excerpt != "Actual rule" || entry.URL != "https://www.notion.so/"+page+"#"+block {
		t.Fatalf("wrong block metadata: %+v", entry)
	}
}

func TestIncompleteNotion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"object":"page_markdown","markdown":"partial","truncated":true,"unknown_block_ids":["unknown"]}`)
	}))
	defer server.Close()
	c := notionClient{server.Client(), server.URL, "secret"}
	if _, err := c.markdown(context.Background(), "page"); err == nil {
		t.Fatal("accepted partial policy")
	}
}

func TestFindingRanges(t *testing.T) {
	lines := addedLines("@@ -1,2 +1,4 @@\n+one\n+two\n context\n+four\n@@ -10 +5 @@\n-old\n+five")
	for _, tt := range []struct {
		name  string
		start *int
		end   int
		want  bool
	}{
		{"single", nil, 1, true},
		{"multiline", gh.Ptr(1), 2, true},
		{"equal", gh.Ptr(2), 2, true},
		{"reversed", gh.Ptr(2), 1, false},
		{"zero", gh.Ptr(0), 2, false},
		{"negative", gh.Ptr(-1), 2, false},
		{"context", gh.Ptr(2), 4, false},
		{"cross hunk", gh.Ptr(4), 5, false},
		{"missing end", nil, 6, false},
		{"missing patch", nil, 0, false},
		{"oversized", gh.Ptr(1), int(^uint(0) >> 1), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := validRange(finding{Line: tt.end, StartLine: tt.start}, lines); got != tt.want {
				t.Fatalf("validRange = %v, want %v", got, tt.want)
			}
		})
	}
	if validRange(finding{Line: 1}, nil) {
		t.Fatal("accepted missing patch")
	}
}

func TestDraftCommentRanges(t *testing.T) {
	for _, start := range []*int{nil, gh.Ptr(10), gh.Ptr(9)} {
		f := finding{Path: "file.go", Line: 10, StartLine: start}
		comment := draftComment(f, "body")
		raw, err := json.Marshal(comment)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if fields["line"] != float64(10) || fields["side"] != "RIGHT" || fields["path"] != "file.go" || fields["body"] != "body" {
			t.Fatalf("wrong comment: %s", raw)
		}
		if start != nil && *start < 10 {
			if fields["start_line"] != float64(9) || fields["start_side"] != "RIGHT" {
				t.Fatalf("missing range fields: %s", raw)
			}
		} else if _, ok := fields["start_line"]; ok {
			t.Fatalf("single-line start_line present: %s", raw)
		} else if _, ok := fields["start_side"]; ok {
			t.Fatalf("single-line start_side present: %s", raw)
		}
	}
}

func TestMultilineSuggestion(t *testing.T) {
	f := finding{Line: 12, Suggestion: gh.Ptr("    first\n    second\n")}
	body, single := renderFinding(f, citation{}, "head", "revision")
	if !strings.Contains(body, "```suggestion\n    first\n    second\n```") {
		t.Fatalf("multiline replacement lost: %s", body)
	}
	f.StartLine = gh.Ptr(11)
	body, ranged := renderFinding(f, citation{}, "head", "revision")
	if single == ranged || !strings.Contains(body, "```suggestion\n    first\n    second\n```") {
		t.Fatal("range marker or replacement incorrect")
	}
	f.StartLine = gh.Ptr(12)
	_, equal := renderFinding(f, citation{}, "head", "revision")
	if single != equal {
		t.Fatal("equivalent single-line locations must deduplicate")
	}
}

func TestStyleRules(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("NOTION_API_KEY", "")
	t.Setenv("NOTION_STYLE_URLS", "")
	t.Setenv("GITHUB_REPOSITORY", "owner/repo")
	t.Setenv("GITHUB_SERVER_URL", "https://github.com")
	cs, text, err := styleRules()
	if err != nil || text != "" || len(cs) != 0 {
		t.Fatalf("absent: %v %q %v", cs, text, err)
	}
	if err := loadNotion(context.Background()); err == nil || !strings.Contains(err.Error(), "No style rules") {
		t.Fatalf("missing sources: %v", err)
	}
	git := func(args ...string) string {
		t.Helper()
		b, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s %v", args, b, err)
		}
		return strings.TrimSpace(string(b))
	}
	git("init")
	if err := os.WriteFile("STYLE.md", []byte("# Errors\n\nPreserve [error] context.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "STYLE.md")
	git("-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "rules")
	sha := git("rev-parse", "HEAD")
	cs, text, err = styleRules()
	if err != nil || len(cs) != 2 || !strings.Contains(text, "Preserve") {
		t.Fatalf("present: %v %v", cs, err)
	}
	c := cs["STYLE:L3"]
	if c.URL != "https://github.com/owner/repo/blob/"+sha+"/STYLE.md#L3" || c.Excerpt != "Preserve [error] context." || !validCitation("STYLE:L3", c) {
		t.Fatalf("wrong citation: %+v", c)
	}
	body, _ := renderFinding(finding{Rule: "Errors"}, c, "head", "revision")
	if !strings.Contains(body, "[Preserve \\[error\\] context.]("+c.URL+")") {
		t.Fatalf("excerpt: %s", body)
	}
	for _, mutate := range []func(*citation){
		func(c *citation) { c.Excerpt = "invented" },
		func(c *citation) { c.URL = strings.ReplaceAll(c.URL, sha, "main") },
		func(c *citation) { c.URL = strings.ReplaceAll(c.URL, "#L3", "#L2") },
		func(c *citation) { c.BlockID = "L2" },
	} {
		bad := c
		mutate(&bad)
		if validCitation("STYLE:L3", bad) {
			t.Fatalf("accepted fabricated citation: %+v", bad)
		}
	}
	if validCitation("STYLE:L2", c) {
		t.Fatal("accepted blank line")
	}
	if err := loadNotion(context.Background()); err != nil {
		t.Fatalf("STYLE-only: %v", err)
	}
	large := strings.Repeat("Rule text. {{literal}}\n", 30000)
	if err := os.WriteFile("STYLE.md", []byte(large), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "STYLE.md")
	git("-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "large rules")
	largeCS, largeText, err := styleRules()
	if err != nil || largeText != large || largeCS["STYLE:L1"].Excerpt != "Rule text. {{literal}}" {
		t.Fatalf("large STYLE: %v", err)
	}
	t.Setenv("GITHUB_ACTIONS", "true")
	output := "outputs.txt"
	if err := os.WriteFile(output, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_OUTPUT", output)
	t.Setenv("RUNNER_TEMP", t.TempDir())
	if err := loadNotion(context.Background()); err != nil {
		t.Fatalf("large file transport: %v", err)
	}
	outputs, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(outputs) > 4096 || strings.Contains(string(outputs), "Rule text") {
		t.Fatalf("inline rules leaked into outputs: %d bytes", len(outputs))
	}
	paths := map[string]string{}
	outputLines := strings.Split(strings.ReplaceAll(string(outputs), "\r\n", "\n"), "\n")
	for i, line := range outputLines {
		key, _, ok := strings.Cut(line, "<<")
		if ok && i+1 < len(outputLines) {
			paths[key] = outputLines[i+1]
		}
	}
	data, err := os.ReadFile(paths["context_file"])
	if err != nil || !strings.Contains(string(data), large) {
		t.Fatalf("full rules file: %v", err)
	}
	metadata, err := readRuleMetadata(paths["metadata_file"])
	if err != nil || metadata.Sources["STYLE"] != "STYLE.md" || metadata.Citations["STYLE:L1"].Excerpt != "Rule text. {{literal}}" {
		t.Fatalf("file metadata: %v", err)
	}
	if paths["context"] != "" || paths["citations_json"] != "" || paths["sources_json"] != "" || len(paths["revision"]) != 64 {
		t.Fatalf("unexpected outputs: %v", paths)
	}
	t.Setenv("GITHUB_ACTIONS", "false")
	t.Setenv("NOTION_STYLE_URLS", "https://www.notion.so/0123456789abcdef0123456789abcdef")
	if err := loadNotion(context.Background()); err == nil || !strings.Contains(err.Error(), "NOTION_API_KEY") {
		t.Fatalf("missing Notion token: %v", err)
	}
	if err := os.WriteFile("STYLE.md", []byte("Modified\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if validCitation("STYLE:L3", c) {
		t.Fatal("accepted changed checkout")
	}
	if _, _, err := styleRules(); err == nil {
		t.Fatal("accepted uncommitted rules")
	}
}

func TestReadRuleMetadata(t *testing.T) {
	for _, data := range []string{"{", "{}", `{"sources":{"N1":"page"}}`} {
		path, err := writeRulesFile("metadata-test-*.json", []byte(data))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Remove(path) })
		if _, err := readRuleMetadata(path); err == nil {
			t.Fatalf("accepted invalid metadata: %s", data)
		}
	}
	if _, err := readRuleMetadata("missing-metadata.json"); err == nil {
		t.Fatal("accepted missing file")
	}
}

func TestLargeNotionRules(t *testing.T) {
	t.Chdir(t.TempDir())
	text := strings.Repeat("a", 5000000)
	page := "0123456789abcdef0123456789abcdef"
	block := "abcdef0123456789abcdef0123456789"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "markdown") {
			json.NewEncoder(w).Encode(map[string]any{"object": "page_markdown", "markdown": text, "truncated": false})
		} else {
			fmt.Fprintf(w, `{"object":"list","results":[{"id":%q,"type":"paragraph","has_children":false,"paragraph":{"rich_text":[{"plain_text":%q}]}}],"has_more":false}`, block, text)
		}
	}))
	defer server.Close()
	c := notionClient{server.Client(), server.URL, "secret"}
	if got, err := c.markdown(context.Background(), page); err != nil || got != text {
		t.Fatalf("large markdown: %v", err)
	}
	cs, err := c.citations(context.Background(), page, "N1")
	if err != nil || cs["N1:"+block].Excerpt != text {
		t.Fatalf("large exact citation: %v", err)
	}
}

func TestConfigCustomModel(t *testing.T) {
	outputs := func(t *testing.T, config string) (map[string]string, error) {
		dir := t.TempDir()
		t.Chdir(dir)
		if err := os.MkdirAll(".github", 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(".github/pi-config.json", []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GITHUB_ACTIONS", "true")
		t.Setenv("GITHUB_OUTPUT", dir+"/outputs.txt")
		if err := os.WriteFile(dir+"/outputs.txt", nil, 0600); err != nil {
			t.Fatal(err)
		}
		if err := run("config"); err != nil {
			return nil, err
		}
		b, err := os.ReadFile(dir + "/outputs.txt")
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		lines := strings.Split(string(b), "\n")
		for i, line := range lines {
			if key, _, ok := strings.Cut(line, "<<"); ok && i+1 < len(lines) {
				got[key] = lines[i+1]
			}
		}
		return got, nil
	}

	got, err := outputs(t, `{"provider":"openai","model":"gpt-5.6-sol","timeout":"60"}`)
	if err != nil || got["model"] != "gpt-5.6-sol" || got["thinking_level"] != "off" || got["provider_base_url"] != "" || got["model_context_window"] != "" {
		t.Fatalf("without custom_model: %v %v", err, got)
	}

	got, err = outputs(t, `{"provider":"openai-custom","model":"gpt-6.1-sol","timeout":"60","thinking_level":"low","custom_model":{
		"base_url":"https://api.openai.com/v1","api":"openai-responses","api_key_env":"OPENAI_API_KEY",
		"reasoning":true,"input":"text","context_window":1050000,"max_tokens":128000}}`)
	want := map[string]string{"provider": "openai-custom", "thinking_level": "low", "provider_base_url": "https://api.openai.com/v1",
		"provider_api": "openai-responses", "provider_api_key": "$OPENAI_API_KEY", "model_reasoning": "true",
		"model_input": "text", "model_context_window": "1050000", "model_max_tokens": "128000"}
	for k, v := range want {
		if err != nil || got[k] != v {
			t.Fatalf("custom_model %s = %q, want %q (err %v)", k, got[k], v, err)
		}
	}

	if _, err = outputs(t, `{"provider":"x","model":"y","timeout":"60","custom_model":{"base_url":"http://x"}}`); err == nil {
		t.Fatal("incomplete custom_model accepted")
	}
	if _, err = outputs(t, `{"provider":"x","model":"y","timeout":"60","thinking_level":"low\"; rm -rf /"}`); err == nil {
		t.Fatal("invalid thinking_level accepted")
	}
}
