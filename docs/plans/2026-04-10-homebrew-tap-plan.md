# Homebrew Tap Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Distribute dexter via Homebrew using a dedicated tap repo with automatic formula updates on each release.

**Architecture:** A separate `remoteoss/homebrew-dexter` repo holds a single `Formula/dexter.rb` that downloads pre-built binaries from GitHub releases. The existing release workflow in this repo gets a new `update-homebrew` job that pushes version/SHA256 updates to the tap after each release.

**Tech Stack:** Homebrew formula (Ruby), GitHub Actions (YAML), shell scripting

**Design doc:** `docs/plans/2026-04-10-homebrew-tap-design.md`

---

### Task 1: Create the Homebrew formula file

This file will be pushed to the `remoteoss/homebrew-dexter` tap repo. We create it locally for reference and for the automation to template from.

**Files:**
- Create: `homebrew/Formula/dexter.rb`

**Step 1: Create the formula**

Create `homebrew/Formula/dexter.rb` with this exact content:

```ruby
class Dexter < Formula
  desc "A lightning-fast Elixir language server"
  homepage "https://github.com/remoteoss/dexter"
  license "MIT"
  version "0.5.3"

  on_macos do
    on_arm do
      url "https://github.com/remoteoss/dexter/releases/download/v0.5.3/dexter_Darwin_arm64.tar.gz"
      sha256 "PLACEHOLDER_DARWIN_ARM64"
    end
  end

  on_linux do
    on_intel do
      url "https://github.com/remoteoss/dexter/releases/download/v0.5.3/dexter_Linux_x86_64.tar.gz"
      sha256 "PLACEHOLDER_LINUX_X86_64"
    end
    on_arm do
      url "https://github.com/remoteoss/dexter/releases/download/v0.5.3/dexter_Linux_arm64.tar.gz"
      sha256 "PLACEHOLDER_LINUX_ARM64"
    end
  end

  def install
    bin.install "dexter"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/dexter version")
  end
end
```

**Step 2: Commit**

```bash
git add homebrew/Formula/dexter.rb
git commit -m "Add Homebrew formula template for tap repo"
```

---

### Task 2: Add the `update-homebrew` job to the release workflow

**Files:**
- Modify: `.github/workflows/release.yml` (append after the `release` job, around line 86)

**Step 1: Add the new job**

Append this job to `.github/workflows/release.yml` after the existing `release` job:

```yaml
  update-homebrew:
    needs: release
    runs-on: ubuntu-latest
    steps:
      - uses: actions/download-artifact@v4
        with:
          path: artifacts
          pattern: dexter-*
          merge-multiple: true

      - name: Compute SHA256s
        id: shas
        run: |
          echo "darwin_arm64=$(sha256sum artifacts/dexter_Darwin_arm64.tar.gz | cut -d' ' -f1)" >> "$GITHUB_OUTPUT"
          echo "linux_x86_64=$(sha256sum artifacts/dexter_Linux_x86_64.tar.gz | cut -d' ' -f1)" >> "$GITHUB_OUTPUT"
          echo "linux_arm64=$(sha256sum artifacts/dexter_Linux_arm64.tar.gz | cut -d' ' -f1)" >> "$GITHUB_OUTPUT"

      - name: Update Homebrew formula
        env:
          TAP_TOKEN: ${{ secrets.HOMEBREW_TAP_TOKEN }}
          VERSION: ${{ inputs.tag && inputs.tag || github.ref_name }}
        run: |
          VERSION="${VERSION#v}"
          git clone https://x-access-token:${TAP_TOKEN}@github.com/remoteoss/homebrew-dexter.git tap
          cd tap

          sed -i \
            -e "s/version \".*\"/version \"${VERSION}\"/" \
            -e "s|/download/v[0-9][^/]*/|/download/v${VERSION}/|g" \
            Formula/dexter.rb

          awk -v d="${{ steps.shas.outputs.darwin_arm64 }}" \
              -v lx="${{ steps.shas.outputs.linux_x86_64 }}" \
              -v la="${{ steps.shas.outputs.linux_arm64 }}" \
            'BEGIN{n=0} /sha256/{n++; if(n==1) sub(/sha256 ".*"/, "sha256 \""d"\"");
             if(n==2) sub(/sha256 ".*"/, "sha256 \""lx"\"");
             if(n==3) sub(/sha256 ".*"/, "sha256 \""la"\"")} {print}' \
            Formula/dexter.rb > Formula/dexter.rb.tmp
          mv Formula/dexter.rb.tmp Formula/dexter.rb

          git config user.name "github-actions[bot]"
          git config user.email "github-actions[bot]@users.noreply.github.com"
          git add Formula/dexter.rb
          git diff --cached --quiet && echo "No changes to formula" && exit 0
          git commit -m "Update dexter to ${VERSION}"
          git push
```

Note the `VERSION` env var uses the same `RELEASE_TAG` logic as the existing workflow — `inputs.tag` for manual dispatch, `github.ref_name` for tag push.

**Step 2: Verify the YAML is valid**

Run: `python3 -c "import yaml; yaml.safe_load(open('.github/workflows/release.yml'))"`
Expected: No output (valid YAML)

**Step 3: Commit**

```bash
git add .github/workflows/release.yml
git commit -m "Add update-homebrew job to release workflow"
```

---

### Task 3: Manual setup checklist (documented, not automated)

These are manual steps the maintainer performs once. No code changes needed — just verify the documentation in the design doc covers them.

**Checklist:**

1. **Create `remoteoss/homebrew-dexter` repo** — public, on GitHub
2. **Push `homebrew/Formula/dexter.rb`** from this repo as `Formula/dexter.rb` in the tap repo (replace PLACEHOLDER SHA256s with real values from the v0.5.3 release `checksums.txt`)
3. **Create a fine-grained PAT** — scoped to `remoteoss/homebrew-dexter`, permissions: Contents (read + write)
4. **Add `HOMEBREW_TAP_TOKEN` secret** — on `remoteoss/dexter` repo under Settings > Secrets and variables > Actions

**Step 1: Add a setup section to the design doc**

Verify `docs/plans/2026-04-10-homebrew-tap-design.md` already has the "Manual Setup (One-Time)" section. It does — no changes needed.

**Step 2: Commit (if any changes were made)**

No commit expected for this task.

---

### Task 4: Test the full flow

This task is done after the manual setup (Task 3) is complete.

**Step 1: Verify `brew tap` works**

```bash
brew tap remoteoss/dexter
```

Expected: taps successfully, no errors.

**Step 2: Verify `brew install` works**

```bash
brew install remoteoss/dexter/dexter
```

Expected: downloads binary, installs to `$(brew --prefix)/bin/dexter`.

**Step 3: Verify the binary runs**

```bash
dexter version
```

Expected: prints `0.5.3`

**Step 4: Verify `brew test` passes**

```bash
brew test dexter
```

Expected: passes (runs `dexter version` and checks output).

**Step 5: Verify auto-update on next release**

Do a test release (or wait for the next real one) and confirm the `update-homebrew` job in GitHub Actions:
- Completes successfully
- The formula in `remoteoss/homebrew-dexter` has the new version and SHA256s
- `brew upgrade dexter` picks up the new version
