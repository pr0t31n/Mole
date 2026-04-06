#!/usr/bin/env python3
"""
install.py — Mole build & dependency installer
===============================================
Detects Linux or macOS and installs all required build tools for Mole.
Mole is a Go project — this script ensures Go and build prerequisites are
present, then builds the Mole binaries for the current platform.

Usage:
    python3 install.py            # install deps + build
    python3 install.py --deps-only  # install deps only, skip build
    python3 install.py --build-only # skip dep install, just build
"""

import argparse
import os
import platform
import shutil
import subprocess
import sys

VERSION = "1.0.0"

# ── Platform detection ────────────────────────────────────────────────────────
SYSTEM = platform.system()
IS_LINUX = SYSTEM == "Linux"
IS_MACOS = SYSTEM == "Darwin"

# Minimum required Go version (major.minor)
MIN_GO_MAJOR = 1
MIN_GO_MINOR = 22

# ── Colours ───────────────────────────────────────────────────────────────────
def _c(code: str, text: str) -> str:
    """Apply ANSI colour if stdout is a terminal."""
    if sys.stdout.isatty():
        return f"\033[{code}m{text}\033[0m"
    return text

def info(msg: str)    -> None: print(_c("36", f"[INFO]  {msg}"))
def ok(msg: str)      -> None: print(_c("32", f"[OK]    {msg}"))
def warn(msg: str)    -> None: print(_c("33", f"[WARN]  {msg}"), file=sys.stderr)
def error(msg: str)   -> None: print(_c("31", f"[ERROR] {msg}"), file=sys.stderr)
def section(msg: str) -> None: print(_c("35", f"\n{'='*60}\n  {msg}\n{'='*60}"))


# ── Shell helpers ─────────────────────────────────────────────────────────────
def run(cmd: list, check: bool = True, env: dict = None) -> subprocess.CompletedProcess:
    """Run a command, stream output, and optionally raise on failure."""
    info(f"Running: {' '.join(cmd)}")
    merged_env = {**os.environ, **(env or {})}
    result = subprocess.run(cmd, env=merged_env)
    if check and result.returncode != 0:
        error(f"Command failed (exit {result.returncode}): {' '.join(cmd)}")
        sys.exit(result.returncode)
    return result


def cmd_exists(name: str) -> bool:
    """Return True if *name* resolves to an executable on PATH."""
    return shutil.which(name) is not None


# ── Go version check ──────────────────────────────────────────────────────────
def check_go_version() -> bool:
    """
    Return True if Go >= MIN_GO_MAJOR.MIN_GO_MINOR is on PATH.
    Prints what was found (or not found).
    """
    if not cmd_exists("go"):
        return False
    result = subprocess.run(["go", "version"], capture_output=True, text=True)
    line = result.stdout.strip()  # e.g. "go version go1.22.3 linux/amd64"
    info(f"Found: {line}")
    parts = line.split()
    if len(parts) < 3:
        return False
    ver = parts[2].lstrip("go")  # "1.22.3"
    try:
        major, minor = int(ver.split(".")[0]), int(ver.split(".")[1])
        return (major, minor) >= (MIN_GO_MAJOR, MIN_GO_MINOR)
    except (IndexError, ValueError):
        return False


# ── Linux: install with apt ───────────────────────────────────────────────────
def install_linux() -> None:
    section("Installing system dependencies (apt)")

    if os.geteuid() != 0:
        warn("Not running as root — apt installs may fail. Consider re-running with sudo.")

    info("Updating apt package list…")
    run(["apt-get", "update", "-y"])

    apt_packages = [
        "git",
        "make",
        "gcc",
        "libc6-dev",
        "curl",
        "wget",
        "ca-certificates",
    ]
    info(f"Installing packages: {', '.join(apt_packages)}")
    run(["apt-get", "install", "-y"] + apt_packages)
    ok("System packages installed.")

    if not check_go_version():
        info("Go not found or version too old — installing Go via official tarball…")
        _install_go_linux()
    else:
        ok(f"Go >= {MIN_GO_MAJOR}.{MIN_GO_MINOR} already available.")


def _install_go_linux() -> None:
    """Download and install the latest stable Go for Linux amd64."""
    GO_VERSION = "1.22.3"
    tarball = f"go{GO_VERSION}.linux-amd64.tar.gz"
    url = f"https://go.dev/dl/{tarball}"

    info(f"Downloading Go {GO_VERSION} from {url}…")
    run(["curl", "-fsSL", "-o", f"/tmp/{tarball}", url])
    run(["rm", "-rf", "/usr/local/go"])
    run(["tar", "-C", "/usr/local", "-xzf", f"/tmp/{tarball}"])
    os.environ["PATH"] = f"/usr/local/go/bin:{os.environ.get('PATH', '')}"
    ok(f"Go {GO_VERSION} installed to /usr/local/go")
    info("Add /usr/local/go/bin to your PATH permanently:")
    info("  echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc")


# ── macOS: install with brew ──────────────────────────────────────────────────
def install_macos() -> None:
    section("Installing system dependencies (brew)")

    if not cmd_exists("brew"):
        warn("Homebrew not found. Installing Homebrew…")
        run([
            "/bin/bash", "-c",
            "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"
        ])

    brew_packages = ["go", "git", "curl", "wget"]
    _brew_user = os.environ.get("SUDO_USER", "")
    _brew_pfx  = ["sudo", "-u", _brew_user] if _brew_user and _brew_user != "root" else []
    for pkg in brew_packages:
        info(f"Installing {pkg} via brew…")
        run(_brew_pfx + ["brew", "install", pkg], check=False)

    if not check_go_version():
        warn(f"Go >= {MIN_GO_MAJOR}.{MIN_GO_MINOR} still not found after brew install.")
        warn("Ensure /opt/homebrew/bin or /usr/local/bin is on your PATH.")
    else:
        ok(f"Go >= {MIN_GO_MAJOR}.{MIN_GO_MINOR} is available.")


# ── Build ─────────────────────────────────────────────────────────────────────
def build() -> None:
    section("Building Mole binaries")

    script_dir = os.path.dirname(os.path.abspath(__file__))
    build_script = os.path.join(script_dir, "build.sh")

    if not os.path.isfile(build_script):
        error(f"build.sh not found at {build_script}")
        sys.exit(1)

    info(f"Running build.sh from {script_dir}…")
    run(["bash", build_script, "all"], env={"GOPATH": os.path.expanduser("~/go")})
    ok("Build complete. Binaries are in ./dist/")


# ── Symlink installation ──────────────────────────────────────────────────────
def _install_symlinks() -> None:
    """Create /usr/local/bin symlinks for all-user access (including root).

    NOTE: Mole is a Go project. Binaries must be built first with:
        bash build.sh all
    Binaries are expected in the dist/ subdirectory of the project root.
    """
    from pathlib import Path as _Path
    import stat as _stat
    app_dir = _Path(__file__).parent.resolve()
    bin_dir = _Path("/usr/local/bin")

    # (binary_relative_to_app_dir, symlink_name_in_bin_dir)
    # Binaries are produced by `bash build.sh all` into dist/
    LINKS = [
        ("dist/mole-client", "mole-client"),
        ("dist/mole-agent",  "mole-agent"),
    ]

    try:
        bin_dir.mkdir(parents=True, exist_ok=True)
    except PermissionError:
        warn("Cannot access /usr/local/bin — run as root to install symlinks.")
        return

    for script_rel, link_name in LINKS:
        script = app_dir / script_rel
        link   = bin_dir / link_name
        if not script.exists():
            warn(f"Binary not found, skipping symlink: {script}  (run: bash build.sh all)")
            continue
        # Make binary executable
        current = script.stat().st_mode
        script.chmod(current | _stat.S_IXUSR | _stat.S_IXGRP | _stat.S_IXOTH)
        # Create/replace symlink
        if link.exists() or link.is_symlink():
            link.unlink()
        link.symlink_to(script)
        ok(f"Symlink installed: {link}  →  {script}")


# ── Entry point ───────────────────────────────────────────────────────────────
def main() -> None:
    parser = argparse.ArgumentParser(
        prog="install.py",
        description=f"Mole installer v{VERSION} — installs build deps and builds Mole binaries.",
    )
    parser.add_argument(
        "--deps-only",
        action="store_true",
        help="Install dependencies only; do not build binaries.",
    )
    parser.add_argument(
        "--build-only",
        action="store_true",
        help="Skip dependency installation; build immediately.",
    )
    args = parser.parse_args()

    print(_c("36", f"""
╔══════════════════════════════════════════════════════╗
║            Mole Installer  v{VERSION}                  ║
║   SOCKS5 + DNS Transparent Proxy Suite               ║
║   Platform: {SYSTEM:<43}║
╚══════════════════════════════════════════════════════╝
"""))

    if not args.build_only:
        if IS_LINUX:
            install_linux()
        elif IS_MACOS:
            install_macos()
        else:
            warn(f"Unsupported platform: {SYSTEM}. Attempting build anyway.")

    if not args.deps_only:
        if not check_go_version():
            error(
                f"Go >= {MIN_GO_MAJOR}.{MIN_GO_MINOR} is required but not found on PATH.\n"
                "  Linux:  Add /usr/local/go/bin to PATH, then re-run.\n"
                "  macOS:  brew install go, then re-run."
            )
            sys.exit(1)
        build()

    _install_symlinks()

    print(_c("32", "\n[DONE]  Mole installation complete.\n"))
    if not args.deps_only:
        print("  Server: copy dist/moled_linux_amd64 to your proxy server and run:")
        print("          sudo bash moled-install.sh")
        print("  Client: copy dist/mole_<os>_<arch> to each client and run:")
        print("          sudo ./mole --config config.yaml")


if __name__ == "__main__":
    main()
