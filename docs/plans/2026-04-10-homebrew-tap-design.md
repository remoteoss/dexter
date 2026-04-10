# Homebrew Tap for Dexter

## Summary

Distribute dexter via Homebrew using a dedicated tap repository (`remoteoss/homebrew-dexter`). The formula downloads pre-built binaries from GitHub releases. The existing release workflow auto-updates the formula on each tagged release.

## User Experience

```sh
brew install remoteoss/dexter/dexter
```

Or:

```sh
brew tap remoteoss/dexter
brew install dexter
```

Supported platforms: macOS arm64, Linux x86_64, Linux arm64.

## Architecture

### Tap Repository (`remoteoss/homebrew-dexter`)

```
homebrew-dexter/
  Formula/
    dexter.rb
```

Single formula file, auto-maintained by CI.

### Formula

```ruby
class Dexter < Formula
  desc "Elixir language server for code intelligence"
  homepage "https://github.com/remoteoss/dexter"
  license "MIT"
  version "0.5.3"

  on_macos do
    on_arm do
      url "https://github.com/remoteoss/dexter/releases/download/v0.5.3/dexter_Darwin_arm64.tar.gz"
      sha256 "PLACEHOLDER"
    end
  end

  on_linux do
    on_intel do
      url "https://github.com/remoteoss/dexter/releases/download/v0.5.3/dexter_Linux_x86_64.tar.gz"
      sha256 "PLACEHOLDER"
    end
    on_arm do
      url "https://github.com/remoteoss/dexter/releases/download/v0.5.3/dexter_Linux_arm64.tar.gz"
      sha256 "PLACEHOLDER"
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

- Pre-built binary download, no build-from-source
- No Intel Mac block (not built in CI)
- `test` block verifies the binary runs

### Auto-Update via Release Workflow

New `update-homebrew` job added to `.github/workflows/release.yml`, runs after the `release` job:

1. Downloads the same build artifacts
2. Computes SHA256 for each platform archive
3. Clones the tap repo using a PAT
4. Updates version, URLs, and SHA256s in the formula via `sed`/`awk`
5. Commits and pushes to the tap repo

Uses `HOMEBREW_TAP_TOKEN` secret (fine-grained PAT scoped to `remoteoss/homebrew-dexter` with Contents write).

## Manual Setup (One-Time)

These steps must be done manually before the automation works:

1. **Create `remoteoss/homebrew-dexter`** — public repo on GitHub
2. **Create a fine-grained PAT** — scoped to `remoteoss/homebrew-dexter`, Contents read+write
3. **Add `HOMEBREW_TAP_TOKEN` secret** — on `remoteoss/dexter` repo (Settings > Secrets > Actions)
4. **Push initial formula** — `Formula/dexter.rb` with v0.5.3 SHA256s computed from the existing release assets

## Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Own tap vs homebrew-core | Own tap | No review process, full control, CGO builds are problematic in homebrew-core |
| Pre-built vs source | Pre-built binary | Avoids requiring Go + C compiler on user machines |
| Auto vs manual update | Auto via CI | Eliminates manual step on each release |
| GoReleaser vs custom workflow | Custom workflow | Existing workflow handles CGO natively per-platform; GoReleaser cross-compile with CGO is painful |
| Formula in tap repo vs main repo | Tap repo | `brew tap` only clones the tap, not the full source tree |
