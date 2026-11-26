# Security policy

## Reporting a vulnerability

Please do **not** open a public issue for security problems. Use GitHub's private reporting:
**Security → Report a vulnerability** on this repository.

Include what you found, how to reproduce it, and the version (tag or commit SHA) you tested.
You will get an answer within a few days; fixes are released as a new patch version of the
current major, and the moving major tag (`v1`) is updated to include them.

## Design notes relevant to reviewers

- The action never runs code from the pull request: it only calls `git diff`/`git cat-file`
  with fixed arguments and full commit SHAs, and never uses a shell. Git (and the lexer worker)
  get a minimal environment, never the step's `INPUT_*` variables or the token; diff drivers,
  textconv, fsmonitor and the pull request's own `.gitattributes` are not used.
- Chroma lexers run in a separate worker process with a per-file and a total time limit, because
  crafted files can make them take quadratic time; hard limits cap files, patch size and run time.
- File names and contents are treated as untrusted. Text derived from them is escaped before it
  reaches workflow commands (`::warning::`), and file names are never written to the Markdown report.
- The configuration is read from the **base** commit, so a pull request cannot change the rules
  it is measured with.
- The token is only sent in the `Authorization` header, over https, to the configured GitHub API
  origin (redirects elsewhere are refused), and is never logged.
- The default workflow uses `pull_request`, not `pull_request_target`.

## Supported versions

Only the latest release of the latest major version receives fixes.
