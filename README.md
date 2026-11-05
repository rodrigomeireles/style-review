# Pi Notion Style Reviewer

A GitHub Actions reviewer that checks pull requests against rules in a root `STYLE.md`, Notion pages, or both, using [`cv/pi-action@v1.10.0`](https://github.com/cv/pi-action).

Each review and `@pi` reply loads the latest configured rules from the trusted PR-base checkout and Notion. Inline findings link the violated rule's verified excerpt to its committed STYLE.md line or exact Notion block.

## Rule sources

Commit a nonempty `STYLE.md` at the repository root, configure Notion pages, or use both. STYLE.md takes precedence over conflicting Notion rules; other Notion rules remain active.

STYLE.md must match the checked-out commit (CRLF conversion is supported). Each nonempty line receives a citation such as `STYLE:L3`, linked to `blob/<checkout SHA>/STYLE.md#L3`. For local use, provide Git and either `GITHUB_REPOSITORY=owner/repo` or a GitHub origin remote. Actions supplies repository metadata automatically; `GITHUB_SERVER_URL` supports HTTPS GitHub Enterprise hosts.

Notion sources must be pages accessible to the integration. The loader uses `Notion-Version: 2026-03-11` and `markdown_version=v2`. All configured sources must load completely; truncated Markdown, unknown blocks, and invalid pagination fail the load. There is no local character or page-count cap (20 or more pages are supported).

## Set up

1. Add your rule sources:
   - **STYLE.md:** commit the file at the repository root.
   - **Notion:** create an integration with **Read content**, then connect it to each style page through the page menu → Connections. Store its token in the Actions secret `NOTION_API_KEY` (or `NOTION_ACCESS_TOKEN`, used when `NOTION_API_KEY` is unset) and set the Actions variable `NOTION_STYLE_URLS` to one page URL per line:

     ```text
     https://www.notion.so/Go-Conventions-0123456789abcdef0123456789abcdef
     https://your-team.notion.site/Architecture-abcdef0123456789abcdef0123456789
     ```

2. Store your model provider's credentials in GitHub Actions secrets. The default provider uses `OPENAI_API_KEY`; both workflows pass it to Pi.
3. Configure `provider`, `model`, and Pi's `timeout` in seconds in `.github/pi-config.json`. Both workflows load this configuration from the trusted PR-base checkout. The current defaults are:

   ```json
   {
     "provider": "openai",
     "model": "gpt-6.1-sol",
     "timeout": "1000"
   }
   ```

   The loader writes complete rules and verified citation JSON to a private temporary file in `RUNNER_TEMP` (the system temporary directory locally). A second private JSON file holds source/citation metadata for the publisher. Only absolute file paths and the small rules revision cross Actions outputs; rules and metadata are not transported in environment variables. Both Pi prompts require reading the entire rules file with existing tools before reviewing or answering. Rules containing template-like text remain file data, not action templates.

   There are no artificial rule-size limits. Reading files still consumes the selected model's actual context window; choose a model that can hold all rules, citation metadata, and review context. Temporary files remain available across job steps and are removed with the runner's job cleanup; local users should remove them after use.

   Keep the workflows' separate `timeout-minutes` job limits large enough for Pi and the surrounding steps.
4. Add `.github/workflows/*.yml`, `cmd/style-review/*.go`, `go.mod`, `go.sum`, and `.github/pi-config.json` to the repository's default branch.
5. Open a same-repository PR, or mention `@pi` in a PR comment as an owner, member, or collaborator.

### Providers

To switch providers, update `provider` and `model`, add the corresponding Actions secret, and pass its API-key environment variable in both workflows. Provider availability follows the Pi version bundled with `cv/pi-action`.

| Provider | API-key environment variable |
|---|---|
| OpenAI | `OPENAI_API_KEY` |
| Anthropic | `ANTHROPIC_API_KEY` |
| Google Gemini | `GEMINI_API_KEY` |
| DeepSeek | `DEEPSEEK_API_KEY` |
| Mistral | `MISTRAL_API_KEY` |
| Groq | `GROQ_API_KEY` |
| Cerebras | `CEREBRAS_API_KEY` |
| xAI | `XAI_API_KEY` |
| OpenRouter | `OPENROUTER_API_KEY` |
| Hugging Face | `HF_TOKEN` |

For Ollama, LM Studio, vLLM, or another OpenAI-compatible server, provision Pi's `models.json` on the runner with the endpoint and a tool-calling model, then select that custom provider and model in `.github/pi-config.json`. Supply authentication as required by the server.

The workflows use GitHub-hosted `ubuntu-latest` runners. Your inference endpoint must be reachable from the runner; for a model on your own machine or network, use a self-hosted runner with access or a securely reachable endpoint.

## How it works

```text
Pull request or @pi mention
  → Checkout trusted PR-base revision
  → Load root STYLE.md and configured Notion pages
  → Write verified rules to a runner file; Pi reads it (STYLE, N1, N2, ...)
  → Pi inspects the code and reviews the diff or answers @pi
  → Publish inline comments, applicable suggestions, or replies
```

- Reviews run for same-repository, ready-for-review PRs on creation, reopen, new commits, and marking ready. `@pi` replies support PR conversation comments, diff comments, and review summaries from authorized collaborators.
- Pi traces relevant entrypoints, calls, dependencies, and consumers, using the full checkout and additional context as needed. It has the runner's available tools and access, including authenticated GitHub CLI requests through `GH_TOKEN`.
- Inline findings identify a rule heading and verified source citation, and target an added PR line. The publisher validates citations and locations, then posts a comment or an **Apply suggestion** block. Replies use the same sources in free-form answers.
- For multiline findings, `start_line` and `line` select an inclusive PR-head range. Every selected line must be added within the same GitHub patch hunk and present in GitHub's supplied patch. Omit `start_line` for a single line; equal endpoints also publish as a single-line finding. Replacement text can contain multiple lines.
- Rules are sent to the selected model provider as prompt context. The Notion token is scoped to the fetch step. To review updated Notion rules, rerun the workflow or push a PR commit.

The workflows build the CLI with Go 1.27.3 through `actions/setup-go` and install Node/npm for the Pi composite action.

## Example agent finding

```json
{
  "findings": [
    {
      "path": "internal/handler/parse.go",
      "line": 43,
      "start_line": 42,
      "source": "N1",
      "citation": "N1:0123456789abcdef0123456789abcdef",
      "rule": "Error handling",
      "message": "Wrap the error to preserve the operation context.",
      "suggestion": "    wrapped := fmt.Errorf(\"parse document: %w\", err)\n    return wrapped"
    }
  ]
}
```

The inline comment links the verified rule excerpt to its exact Notion block.
