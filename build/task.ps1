# build/task.ps1 — Taskfile.yml shim for Windows when `task` (go-task) is not installed.
# Usage: pwsh build/task.ps1 [build|test|lint|demo|ci]
# Each target runs the raw commands documented in Taskfile.yml.

param(
    [Parameter(Position = 0)]
    [ValidateSet('build', 'test', 'lint', 'demo', 'ci', 'help')]
    [string]$Target = 'help'
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
try {
    switch ($Target) {
        'build' {
            & go build ./...
            if ($LASTEXITCODE -ne 0) { throw 'go build failed' }
            & cargo build
            if ($LASTEXITCODE -ne 0) { throw 'cargo build failed' }
        }
        'test' {
            & go test ./...
            if ($LASTEXITCODE -ne 0) { throw 'go test failed' }
            & cargo test
            if ($LASTEXITCODE -ne 0) { throw 'cargo test failed' }
        }
        'lint' {
            $golangci = Get-Command golangci-lint -ErrorAction SilentlyContinue
            if ($golangci) { & golangci-lint run ./... } else { Write-Host 'golangci-lint not installed, skipping' }
            & cargo clippy --all-targets
            if ($LASTEXITCODE -ne 0) { Write-Host 'clippy issues / not installed' }
        }
        'demo' {
            & go run ./test/e2e
            if ($LASTEXITCODE -ne 0) { throw 'e2e demo failed' }
        }
        'ci' {
            & $PSCommandPath build
            & $PSCommandPath test
            & $PSCommandPath lint
        }
        default {
            Write-Host @'
Cerberus task shim (install go-task for full Taskfile.yml):

  pwsh build/task.ps1 build   — go build ./... && cargo build
  pwsh build/task.ps1 test    — go test ./... && cargo test
  pwsh build/task.ps1 lint    — golangci-lint + clippy (best-effort)
  pwsh build/task.ps1 demo    — go run ./test/e2e
  pwsh build/task.ps1 ci      — build + test + lint

Install task: go install github.com/go-task/task/v3/cmd/task@latest
'@
        }
    }
} finally {
    Pop-Location
}
