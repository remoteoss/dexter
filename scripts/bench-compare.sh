#!/usr/bin/env bash
#
# Compare Dexter indexing performance between two git refs.
#
# Usage:
#   scripts/bench-compare ~/code/myapp main feature-branch   # compare two branches
#   scripts/bench-compare ~/code/myapp HEAD~5                # compare a ref against current HEAD
#   scripts/bench-compare ~/code/myapp main feature-branch 10 # 10 runs instead of default 5
#
# Requires: hyperfine (brew install hyperfine)

set -euo pipefail

if [ $# -lt 2 ]; then
  echo "Usage: scripts/bench-compare <project> <ref-a> [ref-b] [runs]" >&2
  echo "" >&2
  echo "  project  Path to the Elixir project to index" >&2
  echo "  ref-a    First git ref to benchmark (branch, tag, commit)" >&2
  echo "  ref-b    Second git ref (default: current HEAD)" >&2
  echo "  runs     Number of runs per ref (default: 5)" >&2
  exit 1
fi

PROJECT="$1"
REF_A="$2"
REF_B="${3:-HEAD}"
RUNS="${4:-5}"
DEXTER_DIR="$(cd "$(dirname "$0")/.." && pwd)"
TMPDIR_BASE="$(mktemp -d)"

cleanup() {
  git -C "$DEXTER_DIR" worktree remove --force "$TMPDIR_BASE/worktree-a" 2>/dev/null || true
  git -C "$DEXTER_DIR" worktree remove --force "$TMPDIR_BASE/worktree-b" 2>/dev/null || true
  rm -rf "$TMPDIR_BASE"
}
trap cleanup EXIT

if ! command -v hyperfine &>/dev/null; then
  echo "Error: hyperfine not found. Install with: brew install hyperfine" >&2
  exit 1
fi

if [ ! -d "$PROJECT" ]; then
  echo "Error: project directory not found: $PROJECT" >&2
  exit 1
fi

# Resolve ref names to short labels for display
label_a=$(git -C "$DEXTER_DIR" rev-parse --short "$REF_A" 2>/dev/null || echo "$REF_A")
label_b=$(git -C "$DEXTER_DIR" rev-parse --short "$REF_B" 2>/dev/null || echo "$REF_B")

# Use branch name if the ref is a branch
if git -C "$DEXTER_DIR" show-ref --verify --quiet "refs/heads/$REF_A" 2>/dev/null; then
  label_a="$REF_A"
fi
if git -C "$DEXTER_DIR" show-ref --verify --quiet "refs/heads/$REF_B" 2>/dev/null; then
  label_b="$REF_B"
fi

binary_a="$TMPDIR_BASE/dexter-a"
binary_b="$TMPDIR_BASE/dexter-b"
echo "Building $label_a ..."
git -C "$DEXTER_DIR" worktree add --quiet --detach "$TMPDIR_BASE/worktree-a" "$REF_A"
(cd "$TMPDIR_BASE/worktree-a" && go build -o "$binary_a" ./cmd/)
git -C "$DEXTER_DIR" worktree remove --force "$TMPDIR_BASE/worktree-a"

echo "Building $label_b ..."
git -C "$DEXTER_DIR" worktree add --quiet --detach "$TMPDIR_BASE/worktree-b" "$REF_B"
(cd "$TMPDIR_BASE/worktree-b" && go build -o "$binary_b" ./cmd/)
git -C "$DEXTER_DIR" worktree remove --force "$TMPDIR_BASE/worktree-b"

echo ""
echo "Benchmarking against: $PROJECT"
echo "Runs per ref: $RUNS"
echo ""

hyperfine \
  --warmup 1 \
  --runs "$RUNS" \
  --command-name "$label_a" "'$binary_a' init --force --profile '$PROJECT'" \
  --command-name "$label_b" "'$binary_b' init --force --profile '$PROJECT'"
