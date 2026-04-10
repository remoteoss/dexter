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
