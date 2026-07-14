#!/usr/bin/env sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
mkdir -p "$root/dist"

case "$(uname -s)" in
  Darwin) output="$root/dist/user-routing.dylib" ;;
  Linux) output="$root/dist/user-routing.so" ;;
  *) echo "unsupported operating system" >&2; exit 1 ;;
esac

CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o "$output" "$root"

