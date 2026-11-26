# PR Code Stats

A GitHub Action that comments on every pull request how many lines it adds and removes, split into
**production code**, **tests**, **comments** and **other** — for any language [Chroma](https://github.com/alecthomas/chroma) can lex.
It is a single comment per PR: pushing new commits updates it instead of adding another.

```text
## 📊 PR Code Statistics
| Files changed | 12   |
| Lines added   | +448 |
| Lines removed | -129 |
| Net change    | +319 |

| Category           | Added | Removed |
| 💻 Production code | 280   | 80      |
| 🧪 Tests           | 110   | 36      |
| 💬 Comments        | 38    | 10      |
| 📄 Other           | 20    | 3       |
| **Total**          | 448   | 129     |
```

## Quick start

`.github/workflows/pr-code-stats.yml` (also in [`examples/pr-analysis.yml`](examples/pr-analysis.yml)):

```yaml
name: PR Code Statistics

on:
  pull_request:
    types: [opened, synchronize, reopened]

permissions:
  contents: read
  pull-requests: write

concurrency:
  group: pr-code-stats-${{ github.event.pull_request.number }}
  cancel-in-progress: true

jobs:
  analyze:
    runs-on: ubuntu-latest
    timeout-minutes: 20
    steps:
      - uses: actions/checkout@v7
        with:
          fetch-depth: 0
          persist-credentials: false

      - uses: afjcjsbx/pr-code-stats@v1
        with:
          github-token: ${{ secrets.GITHUB_TOKEN }}
          base-sha: ${{ github.event.pull_request.base.sha }}
          head-sha: ${{ github.event.pull_request.head.sha }}
```

You do not need Go or any other tool on the runner.

> **Requirements.** This is a Docker container action: it needs a **Linux** runner (`ubuntu-*`).
> Self-hosted runners need Docker installed. The image is built on the runner from this repository's `Dockerfile`
> each time the step runs (about a minute).
>
> **Full history.** `fetch-depth: 0` makes both commits and their merge base available. If a commit is missing,
> the action fails with a message saying so; it never treats missing content as an empty file.

For maximum supply-chain safety pin a full commit SHA instead of the moving major tag:
`uses: afjcjsbx/pr-code-stats@<full-40-char-sha> # v1.0.0`.

## Inputs

| Input | Required | Default | Description |
|---|---|---|---|
| `github-token` | if `comment: true` | – | Token used to create/update the comment. `${{ secrets.GITHUB_TOKEN }}` is enough. |
| `base-sha` | yes | – | `${{ github.event.pull_request.base.sha }}` |
| `head-sha` | yes | – | `${{ github.event.pull_request.head.sha }}` |
| `comment` | no | `true` | `false` computes the statistics without commenting (no token needed). |
| `config` | no | `.github/pr-code-stats.yml` | Configuration file path, read from the **base** commit. |
| `fail-on-comment-error` | no | `false` | Fail the step if the comment cannot be written. |
| `write-step-summary` | no | `true` | Also write the report to the workflow run summary. |

SHAs must be full lowercase hex (40 or 64 characters); branch names and abbreviations are rejected.

## Outputs

| Output | Description |
|---|---|
| `added` | Total lines added |
| `removed` | Total lines removed |
| `net` | `added - removed` |
| `files-changed` | Changed files included in the report (excluded files are not counted) |
| `comment-url` | URL of the created/updated comment, empty if none |
| `comment-status` | `created`, `updated`, `skipped` or `disabled` |

## Permissions

```yaml
permissions:
  contents: read
  pull-requests: write
```

`pull-requests: write` is only needed to write the comment; `issues: write` works too. No write access to contents is ever needed.

### Pull requests from forks

With the `pull_request` event GitHub gives fork PRs a read-only `GITHUB_TOKEN`. The action still analyses the PR,
fills the outputs and the step summary, then logs a warning and sets `comment-status: skipped` — the job stays green unless you set
`fail-on-comment-error: true`. The example workflow deliberately does **not** use `pull_request_target`;
publishing comments on fork PRs would need a separate security design and is not supported.

## What is counted

The diff is `git diff -U0` from the **merge base** of `base-sha` and `head-sha` to `head-sha` — the same
range as the PR's *Files changed* tab, so commits that reached the base branch after you branched are not counted.
A replaced line counts as one removal plus one addition. This is a statistic of the diff, not of the repository.

Every changed line lands in exactly one category, first match wins:

1. **Comments** — the lexer says the line holds only comment tokens (including blank lines *inside* a multi-line comment).
   A line with code *and* a comment is code; `//` or `#` inside a string is code, because the lexer decides, not a text search.
2. **Tests** — a code or blank line of a file recognised as a test (see below).
3. **Production code** — a code or blank line of any other source file with a lexer.
4. **Other** — documentation (`.md`, `.rst`, `.txt`...), data/config formats (JSON, YAML, TOML, INI, XML, CSV...), lock files, `go.mod`,
   files without a lexer, and files too large to analyse. Their comments are not split out.

`Total = Production + Tests + Comments + Other` for both added and removed, always. Binary files count in *Files changed* but add no lines.
Renames use the old path/content for removed lines and the new path/content for added lines; a pure rename is one changed file and zero lines.

A file under `tests/` that is documentation or data (say `tests/fixtures/a.json`) is **Other**: test paths only decide
between *Tests* and *Production code* for source files.

### Test detection

By file name (any depth) — Go `*_test.go`; JS/TS `*.test.*`, `*.spec.*`; Python `test_*.py`, `*_test.py`; Ruby `*_spec.rb`, `*_test.rb`;
Java `Test*.java`, `*Test.java`, `*Tests.java`, `*TestCase.java`; Kotlin `*Test.kt`, `*Tests.kt`; C# `*Test.cs`, `*Tests.cs`, `*TestCase.cs` —
or by directory: `test/`, `tests/`, `__tests__/`, `spec/`, `specs/`. Matching is **case-sensitive**.
Tests written inside production files (Rust `#[cfg(test)]`, doctests...) are not recognised; those lines follow the file's category.

### Known limits

- "All languages" means every lexer Chroma ships. The result is a **lexical** statistic, not semantic analysis.
- Docstrings (e.g. Python `"""..."""`) are string tokens for the lexer, so they count as code, not comments.
- `git diff` can pick a different but equally valid alignment than GitHub's UI on ambiguous changes, so totals may differ from the PR header by a line or two.
- Files bigger than `max_file_size` (default 1 MiB) are not lexed: their lines are counted exactly, but as **Other**, with a warning and a note in the report.
  The same applies if a lexer fails on a file.
- **Hostile files cannot stall the job.** Chroma's lexers can take quadratic time on crafted input (a Java file of 20,000 newlines needs about a minute)
  and cannot be interrupted, so files are lexed in a separate worker process. A file that takes longer than 10 s is cut off (the worker is killed and restarted)
  and counts as **Other** with a warning; all lexing together gets 4 minutes, after which the remaining files count as **Other** too.
  Line counts stay exact either way, and the report says how many files were degraded.
- Refused outright, with an error and no report, because a partial result would be misleading: more than 100,000 changed files, a patch above 1 GiB,
  or an analysis that runs past 15 minutes. Still set `timeout-minutes` on the job as your own backstop.
- `.gitattributes` is read from the **base** commit. A pull request that adds `* -diff` or `binary` to its own `.gitattributes` cannot make its lines disappear from the statistics.
- The statistics describe the diff; do not use them as a security gate. Files under the default excludes (`vendor/`, `dist/`, `build/`, `node_modules/`, `*.min.js`)
  are ignored whatever they contain, and file names decide what counts as a test.

## Configuration

Optional `.github/pr-code-stats.yml`. Missing file = defaults; malformed file = the step fails with the offending field.

```yaml
version: 1            # required

tests:
  directories:        # globs for test directories (added to the built-in ones)
    - "**/e2e/**"
  file_patterns:      # globs for test files (added to the built-in ones)
    - "*.check.sh"

exclude:              # ignored completely: no lines, not in "Files changed" (added to the built-in ones)
  - "**/generated/**"

max_file_size: 1048576  # bytes; larger files are counted as Other (at most 16 MiB)
```

Limits: the file may be at most 64 KiB, each list at most 200 patterns of at most 256 bytes.

Built-in excludes: `**/vendor/**`, `**/node_modules/**`, `**/dist/**`, `**/build/**`, `**/*.min.js`.
Your lists *extend* the built-in ones.

**Glob semantics.** Matched against the slash-separated path from the repository root, case-sensitive:
`*` any characters except `/`; `?` one character except `/`; `[a-z]` / `[!a-z]` classes; `**/` zero or more directories; `**` anything including `/`.
Like `.gitignore`, a pattern without a `/` matches at any depth (`*_test.go` = `**/*_test.go`), a leading `/` anchors it at the root,
and a trailing `/` means everything below that directory. For a rename, each side is checked with its own path.

**GitHub API URL.** The token is only sent over `https`. `GITHUB_API_URL` (set by the runner) must be an https URL; plain `http` is accepted for
loopback hosts only, unless you set the step environment variable `PR_CODE_STATS_ALLOW_HTTP=1` (an http-only GitHub Enterprise Server). Redirects to another origin are refused.

**Security: the config comes from the base commit.** A pull request cannot change the rules used to measure itself.
Config edits take effect after the PR is merged (its own changes to the file are still counted as lines of that PR).

## Development

```sh
go vet ./... && go test ./...                      # needs git; end-to-end tests build throwaway repositories
go test -tags integration -count=1 ./itest/        # needs Docker; builds the image and runs it like the Actions runner
go build ./cmd/pr-code-stats
```

Run it locally on a clone (no comment):

```sh
env 'INPUT_BASE-SHA=<sha>' 'INPUT_HEAD-SHA=<sha>' INPUT_COMMENT=false GITHUB_WORKSPACE=$PWD pr-code-stats
```

| Package | Role |
|---|---|
| `cmd/pr-code-stats` | reads inputs, validates context, orchestrates, writes outputs |
| `internal/analyzer` | git diff parsing, Chroma classification in a killable worker process, caching, aggregation |
| `internal/config` | YAML config and globs |
| `internal/github` | REST client: list, create, update the comment |
| `internal/report` | Markdown |

## Versioning and releases

[Semantic Versioning](https://semver.org). Inputs and outputs are the public API; breaking changes mean a new major.
Pushing a tag `vX.Y.Z` runs the release workflow, which creates the GitHub release and moves the major tag (`v1`) to it.
Enable *immutable releases* in the repository settings so published versions cannot be altered.

## License

[MIT](LICENSE)
