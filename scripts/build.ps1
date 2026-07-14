param(
    [ValidateSet("windows", "linux", "darwin")]
    [string]$Target = "windows"
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
$dist = Join-Path $root "dist"
New-Item -ItemType Directory -Force -Path $dist | Out-Null

$extension = switch ($Target) {
    "windows" { "dll" }
    "linux" { "so" }
    "darwin" { "dylib" }
}

$env:CGO_ENABLED = "1"
$env:GOOS = $Target
$env:GOARCH = "amd64"
if ($Target -eq "windows" -and [string]::IsNullOrWhiteSpace($env:CC)) {
    $localLLVM = Get-ChildItem -Path (Join-Path $root "_tools\llvm-mingw") -Recurse -Filter "x86_64-w64-mingw32-clang.exe" -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($localLLVM) {
        $env:CC = $localLLVM.FullName
    }
}
if ($Target -eq "linux" -and [string]::IsNullOrWhiteSpace($env:CC)) {
    $localZig = Get-ChildItem -Path (Join-Path $root "_tools\zig") -Recurse -Filter "zig.exe" -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($localZig) {
        $env:CC = "$($localZig.FullName) cc -target x86_64-linux-gnu.2.17"
    }
}
go build -trimpath -buildmode=c-shared -o (Join-Path $dist "user-routing.$extension") $root
