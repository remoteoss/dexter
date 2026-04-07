#!/usr/bin/env bash

set -euo pipefail

GH_REPO="https://github.com/remoteoss/dexter"
TOOL_NAME="dexter"
TOOL_TEST="dexter version"

fail() {
	echo -e "asdf-$TOOL_NAME: $*"
	exit 1
}

sort_versions() {
	sed 'h; s/[+-]/./g; s/.p\([[:digit:]]\)/.z\1/; s/$/.z/; G; s/\n/ /' |
		LC_ALL=C sort -t. -k 1,1 -k 2,2n -k 3,3n -k 4,4n -k 5,5n | awk '{print $2}'
}

get_platform() {
	local os arch
	os="$(uname -s)"
	arch="$(uname -m)"

	case "$arch" in
	aarch64) arch="arm64" ;;
	esac

	echo "${os}_${arch}"
}

list_all_versions() {
	git ls-remote --tags --refs "$GH_REPO" |
		grep -o 'refs/tags/.*' | cut -d/ -f3- |
		sed 's/^v//' || fail "Could not list versions from $GH_REPO"
}

download_binary() {
	local version="$1"
	local download_path="$2"
	local platform
	platform="$(get_platform)"

	local url="${GH_REPO}/releases/download/v${version}/dexter_${platform}.tar.gz"

	if curl -fsSL "$url" 2>/dev/null | tar -xz -C "$download_path" --strip-components=1 2>/dev/null; then
		echo "* Downloaded $TOOL_NAME v$version binary for $platform"
		return 0
	fi

	return 1
}

download_source() {
	local version="$1"
	local download_path="$2"

	echo "* Pre-built binary not available, cloning source..."
	git clone --depth 1 --branch "v${version}" "$GH_REPO" "$download_path" 2>/dev/null ||
		git clone --depth 1 --branch "${version}" "$GH_REPO" "$download_path" ||
		fail "Could not clone $GH_REPO at version $version"
}

download_release() {
	local version="$1"
	local download_path="$2"

	download_binary "$version" "$download_path" || download_source "$version" "$download_path"
}

install_version() {
	local install_type="$1"
	local version="$2"
	local install_path="${3%/bin}/bin"

	if [ "$install_type" != "version" ]; then
		fail "asdf-$TOOL_NAME supports release installs only"
	fi

	(
		mkdir -p "$install_path"

		if [ -f "$ASDF_DOWNLOAD_PATH/$TOOL_NAME" ]; then
			echo "* Installing pre-built $TOOL_NAME v$version..."
			cp "$ASDF_DOWNLOAD_PATH/$TOOL_NAME" "$install_path/$TOOL_NAME"
			chmod +x "$install_path/$TOOL_NAME"
		else
			command -v go >/dev/null 2>&1 || fail "Go is required for source builds. Install via mise: mise use -g go@1.26.1"
			command -v cc >/dev/null 2>&1 || fail "A C compiler is required for source builds (SQLite). On macOS, install Xcode Command Line Tools: xcode-select --install"

			echo "* Building $TOOL_NAME v$version from source..."
			cd "$ASDF_DOWNLOAD_PATH"
			CGO_ENABLED=1 go build -o "$install_path/$TOOL_NAME" ./cmd/
		fi

		local tool_cmd
		tool_cmd="$(echo "$TOOL_TEST" | cut -d' ' -f1)"
		test -x "$install_path/$tool_cmd" || fail "Expected $install_path/$tool_cmd to be executable."

		echo "$TOOL_NAME $version installation was successful!"
	) || (
		rm -rf "$install_path"
		fail "An error occurred while installing $TOOL_NAME $version."
	)
}
