#!/usr/bin/env bash
#
# Updates the Homebrew formula in remoteoss/homebrew-tap with new version and SHA256s.
#
# Required environment variables:
#   TAP_TOKEN      - GitHub PAT with write access to remoteoss/homebrew-tap
#   VERSION        - Release tag (e.g. v0.5.3, the v prefix is stripped automatically)
#   ARTIFACTS_DIR  - Directory containing the release tar.gz archives

set -euo pipefail

VERSION="${VERSION#v}"

SHA_DARWIN_ARM64=$(sha256sum "${ARTIFACTS_DIR}/dexter_Darwin_arm64.tar.gz" | cut -d' ' -f1)
SHA_LINUX_X86_64=$(sha256sum "${ARTIFACTS_DIR}/dexter_Linux_x86_64.tar.gz" | cut -d' ' -f1)
SHA_LINUX_ARM64=$(sha256sum "${ARTIFACTS_DIR}/dexter_Linux_arm64.tar.gz" | cut -d' ' -f1)

git clone "https://x-access-token:${TAP_TOKEN}@github.com/remoteoss/homebrew-tap.git" tap
cd tap

sed -i \
  -e "s/version \".*\"/version \"${VERSION}\"/" \
  -e "s|/download/v[0-9][^/]*/|/download/v${VERSION}/|g" \
  Formula/dexter.rb

awk -v d="$SHA_DARWIN_ARM64" \
    -v lx="$SHA_LINUX_X86_64" \
    -v la="$SHA_LINUX_ARM64" \
  'BEGIN{n=0} /sha256/{n++; if(n==1) sub(/sha256 ".*"/, "sha256 \""d"\"");
   if(n==2) sub(/sha256 ".*"/, "sha256 \""lx"\"");
   if(n==3) sub(/sha256 ".*"/, "sha256 \""la"\"")} {print}' \
  Formula/dexter.rb > Formula/dexter.rb.tmp
mv Formula/dexter.rb.tmp Formula/dexter.rb

git config user.name "github-actions[bot]"
git config user.email "github-actions[bot]@users.noreply.github.com"

BRANCH="update-dexter-${VERSION}"
git checkout -b "$BRANCH"
git add Formula/dexter.rb
git diff --cached --quiet && echo "No changes to formula" && exit 0
git commit -m "Update dexter to ${VERSION}"
git push origin "$BRANCH"

GH_TOKEN="$TAP_TOKEN" gh pr create \
  --repo remoteoss/homebrew-tap \
  --base main \
  --head "$BRANCH" \
  --title "Update dexter to ${VERSION}" \
  --body "Automated PR from [dexter release v${VERSION}](https://github.com/remoteoss/dexter/releases/tag/v${VERSION})."
