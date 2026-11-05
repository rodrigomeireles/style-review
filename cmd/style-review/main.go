package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/actions-go/toolkit/core"
	actions "github.com/actions-go/toolkit/github"
	gh "github.com/google/go-github/v74/github"
)

func main() {
	if len(os.Args) != 2 {
		core.SetFailed("Expected config, load-notion, resolve-pr, publish-review or publish-reply")
		os.Exit(1)
	}
	if err := run(os.Args[1]); err != nil {
		core.SetFailed(err.Error())
		os.Exit(1)
	}
}

func run(command string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	switch command {
	case "config":
		b, err := os.ReadFile(".github/pi-config.json")
		if err != nil {
			return err
		}
		var c struct {
			Provider string          `json:"provider"`
			Model    string          `json:"model"`
			Timeout  json.RawMessage `json:"timeout"`
			// ThinkingLevel replaces pi-action's hardcoded "off" (see the workflows' prepare step).
			ThinkingLevel string `json:"thinking_level"`
			// CustomModel registers a model the Pi bundled in cv/pi-action does not know yet.
			CustomModel *struct {
				BaseURL       string `json:"base_url"`
				API           string `json:"api"`
				APIKeyEnv     string `json:"api_key_env"`
				Reasoning     bool   `json:"reasoning"`
				Input         string `json:"input"`
				ContextWindow int    `json:"context_window"`
				MaxTokens     int    `json:"max_tokens"`
			} `json:"custom_model"`
		}
		if err = json.Unmarshal(b, &c); err != nil {
			return err
		}
		timeout := strings.Trim(string(c.Timeout), "\"")
		n, err := strconv.Atoi(timeout)
		if strings.TrimSpace(c.Provider) == "" || strings.TrimSpace(c.Model) == "" || err != nil || n <= 0 {
			return errors.New("Invalid Pi configuration")
		}
		thinking := c.ThinkingLevel
		if thinking == "" {
			thinking = "off"
		}
		if !regexp.MustCompile(`^(off|minimal|low|medium|high|xhigh|max)$`).MatchString(thinking) {
			return errors.New("Invalid Pi thinking_level: use off, minimal, low, medium, high, xhigh or max")
		}
		// Empty outputs leave pi-action's defaults (no custom provider) in place.
		custom := map[string]string{"provider_base_url": "", "provider_api": "", "provider_api_key": "",
			"model_reasoning": "", "model_input": "", "model_context_window": "", "model_max_tokens": ""}
		if m := c.CustomModel; m != nil {
			if !strings.HasPrefix(m.BaseURL, "https://") || m.API == "" || m.APIKeyEnv == "" || m.ContextWindow <= 0 || m.MaxTokens <= 0 {
				return errors.New("Invalid Pi custom_model: base_url (https), api, api_key_env, context_window and max_tokens are required")
			}
			custom = map[string]string{"provider_base_url": m.BaseURL, "provider_api": m.API,
				// Pi treats "$NAME" as an env reference; a bare NAME would be sent as the literal key.
				"provider_api_key": "$" + m.APIKeyEnv,
				"model_reasoning":  strconv.FormatBool(m.Reasoning), "model_input": m.Input,
				"model_context_window": strconv.Itoa(m.ContextWindow), "model_max_tokens": strconv.Itoa(m.MaxTokens)}
		}
		core.SetOutput("provider", c.Provider)
		core.SetOutput("model", c.Model)
		core.SetOutput("timeout", timeout)
		core.SetOutput("thinking_level", thinking)
		for k, v := range custom {
			core.SetOutput(k, v)
		}
		return nil
	case "load-notion", "load-rules":
		return loadNotion(ctx)
	case "resolve-pr", "publish-review", "publish-reply":
		a := actions.ParseActionEnv()
		if a.Repo.Owner == "" || a.Repo.Repo == "" || a.Issue.Number <= 0 {
			return errors.New("Expected a PR event")
		}
		client := actions.NewClient()
		if command == "resolve-pr" {
			pr, _, err := client.PullRequests.Get(ctx, a.Repo.Owner, a.Repo.Repo, a.Issue.Number)
			if err != nil {
				return err
			}
			core.SetOutput("number", strconv.Itoa(a.Issue.Number))
			core.SetOutput("base_sha", pr.GetBase().GetSHA())
			return nil
		}
		if os.Getenv("PI_SUCCESS") != "true" {
			return errors.New("Pi invocation failed")
		}
		if command == "publish-reply" {
			return publishReply(ctx, client, a)
		}
		return publishReview(ctx, client, a)
	default:
		return errors.New("Unknown subcommand")
	}
}

func publishReply(ctx context.Context, c *gh.Client, a actions.ActionContext) error {
	body := strings.TrimSpace(os.Getenv("PI_RESPONSE"))
	if body == "" {
		return errors.New("Pi returned an empty reply")
	}
	if a.EventName == "pull_request_review_comment" {
		b, err := os.ReadFile(os.Getenv("GITHUB_EVENT_PATH"))
		if err != nil {
			return err
		}
		var event struct {
			Comment struct {
				ID   int64 `json:"id"`
				Root int64 `json:"in_reply_to_id"`
			} `json:"comment"`
		}
		if err = json.Unmarshal(b, &event); err != nil {
			return err
		}
		id := event.Comment.Root
		if id == 0 {
			id = event.Comment.ID
		}
		if id == 0 {
			return errors.New("Missing review comment ID")
		}
		_, _, err = c.PullRequests.CreateCommentInReplyTo(ctx, a.Repo.Owner, a.Repo.Repo, a.Issue.Number, body, id)
		return err
	}
	_, _, err := c.Issues.CreateComment(ctx, a.Repo.Owner, a.Repo.Repo, a.Issue.Number, &gh.IssueComment{Body: &body})
	return err
}

type finding struct {
	Path       string  `json:"path"`
	Line       int     `json:"line"`
	StartLine  *int    `json:"start_line,omitempty"`
	Source     string  `json:"source"`
	Citation   string  `json:"citation"`
	Rule       string  `json:"rule"`
	Message    string  `json:"message"`
	Suggestion *string `json:"suggestion"`
}

var hunk = regexp.MustCompile(`\+(\d+)(?:,\d+)?\s*@@`)

func addedLines(patch string) map[int]int {
	added := map[int]int{}
	next, group := -1, 0
	for _, line := range strings.Split(patch, "\n") {
		if strings.HasPrefix(line, "@@") {
			group++
			m := hunk.FindStringSubmatch(line)
			next = -1
			if m != nil {
				next, _ = strconv.Atoi(m[1])
			}
		} else if next >= 0 && strings.HasPrefix(line, "+") {
			added[next] = group
			next++
		} else if next >= 0 && strings.HasPrefix(line, " ") {
			next++
		}
	}
	return added
}

func validRange(f finding, lines map[int]int) bool {
	if f.Line <= 0 || lines[f.Line] == 0 {
		return false
	}
	if f.StartLine == nil {
		return true
	}
	start := *f.StartLine
	if start <= 0 || start > f.Line || f.Line-start >= len(lines) {
		return false
	}
	for n := start; n <= f.Line; n++ {
		if lines[n] != lines[f.Line] {
			return false
		}
	}
	return true
}

// alreadyCommented reports whether a comment on path already raised this rule.
// Comments from before path+citation markers are matched by the rule's link.
func alreadyCommented(existing []*gh.PullRequestComment, path, marker, ruleURL string) bool {
	for _, c := range existing {
		if c.GetPath() == path && (strings.Contains(c.GetBody(), marker) || strings.Contains(c.GetBody(), "]("+ruleURL+")")) {
			return true
		}
	}
	return false
}

func draftComment(f finding, body string) *gh.DraftReviewComment {
	comment := &gh.DraftReviewComment{Path: gh.Ptr(f.Path), Line: gh.Ptr(f.Line), Side: gh.Ptr("RIGHT"), Body: &body}
	if f.StartLine != nil && *f.StartLine < f.Line {
		comment.StartLine = f.StartLine
		comment.StartSide = gh.Ptr("RIGHT")
	}
	return comment
}

func markdownText(s string) string {
	r := strings.NewReplacer("\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]", "<", "&lt;", ">", "&gt;", "\n", " ", "\r", " ")
	return r.Replace(s)
}

// renderFinding returns the comment body and a dedupe marker. The marker covers
// only the file and cited rule: Pi's wording and chosen line vary between runs
// (even on the same commit), so including them let every run repeat itself.
func renderFinding(f finding, c citation) (string, string) {
	hash := sha256.Sum256([]byte(f.Path + "\x00" + f.Citation))
	marker := fmt.Sprintf("<!-- pi-style:%x -->", hash[:10])
	body := fmt.Sprintf("**Style — %s**\n\n[%s](%s)\n\n%s", markdownText(f.Rule), markdownText(c.Excerpt), c.URL, f.Message)
	if f.Suggestion != nil {
		s := *f.Suggestion
		if strings.TrimSpace(s) != "" && len(s) < 4000 && !strings.Contains(s, "```") {
			body += "\n\n```suggestion\n" + strings.TrimSuffix(s, "\n") + "\n```"
		}
	}
	return body + "\n\n" + marker, marker
}

func publishReview(ctx context.Context, c *gh.Client, a actions.ActionContext) error {
	raw := strings.TrimSpace(os.Getenv("PI_RESPONSE"))
	if strings.HasPrefix(raw, "```") {
		if i := strings.Index(raw, "\n"); i >= 0 {
			raw = raw[i+1:]
		}
		raw = strings.TrimSuffix(strings.TrimSpace(raw), "```")
	}
	var review struct {
		Findings []json.RawMessage `json:"findings"`
	}
	if err := json.Unmarshal([]byte(raw), &review); err != nil {
		return err
	}
	if review.Findings == nil {
		return errors.New("Pi response must contain a findings array")
	}
	metadata, err := readRuleMetadata(os.Getenv("PI_RULES_METADATA_FILE"))
	if err != nil {
		return err
	}
	owner, repo, n := a.Repo.Owner, a.Repo.Repo, a.Issue.Number
	pr, _, err := c.PullRequests.Get(ctx, owner, repo, n)
	if err != nil {
		return err
	}
	if a.Payload.PullRequest == nil || pr.GetHead().GetSHA() != a.Payload.PullRequest.GetHead().GetSHA() {
		core.Warning("PR head changed during review; skipping stale comments")
		return nil
	}
	lines := map[string]map[int]int{}
	opts := &gh.ListOptions{PerPage: 100}
	for {
		files, r, e := c.PullRequests.ListFiles(ctx, owner, repo, n, opts)
		if e != nil {
			return e
		}
		for _, f := range files {
			lines[f.GetFilename()] = addedLines(f.GetPatch())
		}
		if r.NextPage == 0 {
			break
		}
		opts.Page = r.NextPage
	}
	var existing []*gh.PullRequestComment
	opts = &gh.ListOptions{PerPage: 100}
	for {
		comments, r, e := c.PullRequests.ListComments(ctx, owner, repo, n, &gh.PullRequestListCommentsOptions{ListOptions: *opts})
		if e != nil {
			return e
		}
		existing = append(existing, comments...)
		if r.NextPage == 0 {
			break
		}
		opts.Page = r.NextPage
	}
	revision := os.Getenv("PI_NOTION_REVISION")
	if len(revision) > 12 {
		revision = revision[:12]
	}
	comments := []*gh.DraftReviewComment{}
	seen := map[string]bool{}
	for i, raw := range review.Findings {
		if i >= 50 {
			break
		}
		var f finding
		if json.Unmarshal(raw, &f) != nil {
			core.Warning("Skipping malformed finding")
			continue
		}
		cite, ok := metadata.Citations[f.Citation]
		if !ok || !validCitation(f.Citation, cite) || cite.Source != f.Source || !validRange(f, lines[f.Path]) || strings.TrimSpace(f.Rule) == "" || strings.TrimSpace(f.Message) == "" {
			core.Warning("Skipping invalid, uncited or out-of-diff finding")
			continue
		}
		body, marker := renderFinding(f, cite)
		if seen[marker] || alreadyCommented(existing, f.Path, marker, cite.URL) {
			core.Info(fmt.Sprintf("Skipping repeat of %s on %s", f.Citation, f.Path))
			continue
		}
		seen[marker] = true
		comments = append(comments, draftComment(f, body))
	}
	if len(comments) == 0 {
		core.Info("No new, valid Style findings")
		return nil
	}
	_, _, err = c.PullRequests.CreateReview(ctx, owner, repo, n, &gh.PullRequestReviewRequest{CommitID: gh.Ptr(pr.GetHead().GetSHA()), Event: gh.Ptr("COMMENT"), Body: gh.Ptr(fmt.Sprintf("Style review (%s): %d finding(s).", revision, len(comments))), Comments: comments})
	return err
}
