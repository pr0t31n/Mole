#!/usr/bin/env python3
"""
Mole — Go TCP/DNS transparent proxy
Install script for /opt/pr0t31n/Mole/
"""

import argparse
import os
import platform
import shutil
import subprocess
import sys
import tarfile
import urllib.request
from pathlib import Path

# ─── ANSI colours ────────────────────────────────────────────────────────────
R  = "\033[1;31m"
G  = "\033[1;32m"
Y  = "\033[1;33m"
B  = "\033[1;34m"
C  = "\033[1;36m"
W  = "\033[0m"
# ─────────────────────────────────────────────────────────────────────────────

APP_NAME    = "Mole"
APP_DIR     = Path("/opt/pr0t31n/Mole")
DIST_DIR    = APP_DIR / "dist"
BIN_DIR     = Path("/usr/local/bin")

GO_MIN      = (1, 22)
GO_VERSION  = "1.22.5"
GO_LINUX    = f"https://go.dev/dl/go{GO_VERSION}.linux-amd64.tar.gz"
GO_MACOS    = f"https://go.dev/dl/go{GO_VERSION}.darwin-amd64.tar.gz"
GO_INSTALL  = Path("/usr/local/go")

SYMLINKS = {
    "mole-client": DIST_DIR / "mole-client",
    "mole-agent":  DIST_DIR / "mole-agent",
}

# system package lists --------------------------------------------------------
APT_PKGS    = ["golang-go", "git", "make", "gcc", "libc6-dev",
               "curl", "wget", "ca-certificates", "build-essential"]
DNF_PKGS    = ["golang", "git", "make", "gcc", "glibc-devel",
               "curl", "wget", "ca-certificates"]
ZYPPER_PKGS = ["go", "git", "make", "gcc", "glibc-devel",
               "curl", "wget", "ca-certificates"]
BREW_PKGS   = ["go", "git", "curl", "wget"]

# ─── helpers ─────────────────────────────────────────────────────────────────

def banner():
    print(f"""
{C}╔══════════════════════════════════════════════╗
║        Mole — Go TCP/DNS Transparent Proxy   ║
║        Install Script                        ║
╚══════════════════════════════════════════════╝{W}
""")


def info(msg):  print(f"{G}[+]{W} {msg}")
def warn(msg):  print(f"{Y}[!]{W} {msg}")
def note(msg):  print(f"{Y}[i]{W} {msg}")
def err(msg):   print(f"{R}[✗]{W} {msg}")
def step(msg):  print(f"\n{B}━━ {msg} ━━{W}")


def run(cmd, **kwargs):
    """Run a command; raise on non-zero exit."""
    print(f"    {C}>{W} {' '.join(str(c) for c in cmd)}")
    result = subprocess.run(cmd, **kwargs)
    if result.returncode != 0:
        raise RuntimeError(f"Command failed: {' '.join(str(c) for c in cmd)}")
    return result


def run_ok(cmd, **kwargs):
    """Run a command; return True on success, False on failure (no raise)."""
    try:
        subprocess.run(cmd, check=True,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                       **kwargs)
        return True
    except (subprocess.CalledProcessError, FileNotFoundError):
        return False


def require_root():
    if platform.system() != "Darwin" and os.geteuid() != 0:
        err("This script must be run as root (or with sudo) on Linux.")
        sys.exit(1)


def detect_os():
    system = platform.system()
    if system == "Darwin":
        return "macos"
    if system == "Windows":
        return "windows"
    # Linux — read /etc/os-release
    os_release = {}
    try:
        with open("/etc/os-release") as f:
            for line in f:
                line = line.strip()
                if "=" in line:
                    k, v = line.split("=", 1)
                    os_release[k] = v.strip('"')
    except FileNotFoundError:
        pass
    dist_id = os_release.get("ID", "").lower()
    id_like  = os_release.get("ID_LIKE", "").lower()
    if dist_id in ("kali", "ubuntu", "debian") or "debian" in id_like:
        return "apt"
    if dist_id in ("rhel", "centos", "rocky", "almalinux", "fedora") or "rhel" in id_like or "fedora" in id_like:
        return "dnf"
    if dist_id in ("sles", "opensuse", "opensuse-leap", "opensuse-tumbleweed") or "suse" in id_like:
        return "zypper"
    return "apt"   # fallback


def go_current_version():
    """Return (major, minor) tuple of installed go, or (0,0) if not found."""
    go_bin = shutil.which("go") or str(GO_INSTALL / "bin" / "go")
    try:
        out = subprocess.check_output([go_bin, "version"],
                                      stderr=subprocess.DEVNULL).decode()
        # e.g. "go version go1.22.5 linux/amd64"
        tok = [t for t in out.split() if t.startswith("go1.")]
        if tok:
            parts = tok[0].lstrip("go").split(".")
            return (int(parts[0]), int(parts[1]))
    except Exception:
        pass
    return (0, 0)


def go_bin():
    """Return path to go binary, preferring /usr/local/go/bin/go."""
    local = GO_INSTALL / "bin" / "go"
    if local.exists():
        return str(local)
    found = shutil.which("go")
    return found or "go"


# ─── phase: system packages ──────────────────────────────────────────────────

def install_system_packages(os_type, args):
    step("Phase 1 — System Packages")
    if args.skip_apt:
        note("--skip-system set; skipping system package install.")
        return

    if os_type == "windows":
        warn("Windows detected. Please install Go 1.22+, git, make, gcc manually.")
        return

    if os_type == "macos":
        _brew_install(BREW_PKGS)
        return

    if os_type == "apt":
        info("Running apt update + apt upgrade …")
        run(["apt-get", "update", "-y"])
        run(["apt-get", "upgrade", "-y"])
        info("Installing APT packages …")
        run(["apt-get", "install", "-y"] + APT_PKGS)

    elif os_type == "dnf":
        info("Installing DNF packages …")
        run(["dnf", "install", "-y"] + DNF_PKGS)

    elif os_type == "zypper":
        info("Installing zypper packages …")
        run(["zypper", "install", "-y"] + ZYPPER_PKGS)


def _brew_installed_set():
    try:
        r = subprocess.run(["brew", "list", "--formula", "-1"],
                           capture_output=True, text=True, check=True)
        return set(r.stdout.strip().splitlines())
    except (subprocess.CalledProcessError, FileNotFoundError):
        return set()


def _brew_install(pkgs):
    brew_path = shutil.which("brew")
    if not brew_path:
        note("Homebrew not found; skipping brew installs.")
        return
    installed = _brew_installed_set()
    SUDO_USER = os.environ.get("SUDO_USER", "")
    for pkg in pkgs:
        if pkg in installed:
            info(f"  ✓ {pkg} already installed")
            continue
        if os.getuid() == 0:
            if not SUDO_USER:
                note(f"Running as root with no SUDO_USER — cannot brew install {pkg}; skipping.")
                continue
            run(["sudo", "-u", SUDO_USER, brew_path, "install", pkg])
        else:
            run([brew_path, "install", pkg])


# ─── Go version guard / tarball install ──────────────────────────────────────

def ensure_go_version(os_type):
    """Verify go >= 1.22; download tarball if package manager gave an older version."""
    step("Go Version Verification")
    ver = go_current_version()
    info(f"Detected Go version: {ver[0]}.{ver[1]}")
    if ver >= GO_MIN:
        info(f"Go {ver[0]}.{ver[1]} satisfies >= {GO_MIN[0]}.{GO_MIN[1]} ✓")
        return

    warn(f"Go {ver[0]}.{ver[1]} < {GO_MIN[0]}.{GO_MIN[1]}; downloading Go {GO_VERSION} tarball …")
    if os_type == "windows":
        err("Cannot auto-install Go tarball on Windows. Please install Go 1.22+ manually from https://go.dev/dl/")
        sys.exit(1)

    url = GO_MACOS if os_type == "macos" else GO_LINUX
    tarball = Path(f"/tmp/go{GO_VERSION}.tar.gz")

    info(f"Downloading {url} …")
    urllib.request.urlretrieve(url, tarball)

    if GO_INSTALL.exists():
        info(f"Removing old {GO_INSTALL} …")
        shutil.rmtree(GO_INSTALL)

    info(f"Extracting to {GO_INSTALL.parent} …")
    with tarfile.open(tarball, "r:gz") as tf:
        tf.extractall(GO_INSTALL.parent)

    tarball.unlink(missing_ok=True)

    # Add to current process PATH so subsequent calls work
    go_bin_dir = str(GO_INSTALL / "bin")
    os.environ["PATH"] = go_bin_dir + ":" + os.environ.get("PATH", "")

    ver = go_current_version()
    if ver < GO_MIN:
        err(f"Go version still {ver[0]}.{ver[1]} after tarball install. Aborting.")
        sys.exit(1)
    info(f"Go {ver[0]}.{ver[1]} installed successfully ✓")


# ─── phase: pip ──────────────────────────────────────────────────────────────

def install_pip_packages(args):
    step("Phase 2 — Pip Packages")
    if args.skip_pip:
        note("--skip-pip set; skipping.")
        return
    # Mole has no Python dependencies
    info("Mole has no Python package dependencies — phase skipped.")


# ─── phase: go (go.mod deps + go mod download) ───────────────────────────────

def install_go_deps(args):
    step("Phase 3 — Go Module Dependencies")
    if args.skip_go:
        note("--skip-go set; skipping.")
        return
    if not APP_DIR.exists():
        warn(f"{APP_DIR} not found; cannot run go mod download.")
        return
    gb = go_bin()
    gopath = Path.home() / "go"
    env = {**os.environ, "GOPATH": str(gopath), "PATH": str(GO_INSTALL / "bin") + ":" + os.environ.get("PATH", "")}
    info("Running go mod download …")
    try:
        run([gb, "mod", "download"], cwd=APP_DIR, env=env)
    except RuntimeError as exc:
        warn(f"go mod download failed (non-fatal): {exc}")


# ─── phase: git (build) ──────────────────────────────────────────────────────

def build_app(args):
    step("Phase 4 — Build (bash build.sh all)")
    if args.skip_git:
        note("--skip-git set; skipping build.")
        return
    if not APP_DIR.exists():
        err(f"App directory {APP_DIR} not found. Clone Mole first.")
        sys.exit(1)
    build_script = APP_DIR / "build.sh"
    if not build_script.exists():
        err(f"build.sh not found in {APP_DIR}.")
        sys.exit(1)
    gopath = Path.home() / "go"
    env = {
        **os.environ,
        "GOPATH":   str(gopath),
        "PATH":     str(GO_INSTALL / "bin") + ":" + os.environ.get("PATH", ""),
        "HOME":     str(Path.home()),
    }
    info("Running bash build.sh all …")
    run(["bash", "build.sh", "all"], cwd=APP_DIR, env=env)


# ─── symlinks ────────────────────────────────────────────────────────────────

def install_symlinks():
    step("Creating Symlinks")
    BIN_DIR.mkdir(parents=True, exist_ok=True)
    for link_name, target in SYMLINKS.items():
        link_path = BIN_DIR / link_name
        if not target.exists():
            note(f"Target {target} not found; skipping symlink {link_name}.")
            continue
        if link_path.is_symlink() or link_path.exists():
            link_path.unlink()
        link_path.symlink_to(target)
        info(f"Symlink: {link_path} → {target}")


# ─── check mode ──────────────────────────────────────────────────────────────

def run_check():
    step("Check Mode — Dependency Status")
    checks = {
        "go":    shutil.which("go") or str(GO_INSTALL / "bin" / "go"),
        "git":   shutil.which("git"),
        "make":  shutil.which("make"),
        "gcc":   shutil.which("gcc"),
    }
    all_ok = True
    for name, path in checks.items():
        if path and Path(path).exists():
            info(f"{name:20s} {G}OK{W}  ({path})")
        else:
            err(f"{name:20s} {R}MISSING{W}")
            all_ok = False

    # Go version
    ver = go_current_version()
    if ver >= GO_MIN:
        info(f"{'go version':20s} {G}OK{W}  ({ver[0]}.{ver[1]})")
    else:
        err(f"{'go version':20s} {R}INSUFFICIENT{W} ({ver[0]}.{ver[1]} < {GO_MIN[0]}.{GO_MIN[1]})")
        all_ok = False

    # Binaries after build
    for name, target in SYMLINKS.items():
        if target.exists():
            info(f"{name:20s} {G}BUILT{W}  ({target})")
        else:
            warn(f"{name:20s} {Y}NOT BUILT{W}  ({target})")

    print()
    if all_ok:
        info(f"{G}All checks passed.{W}")
    else:
        warn(f"{Y}Some checks failed — re-run without --check to install.{W}")
    return all_ok


# ─── post-install summary ─────────────────────────────────────────────────────

def post_install_summary():
    step("Post-Install Verification Summary")
    results = []

    # Go binary
    gb = go_bin()
    ver = go_current_version()
    ok = ver >= GO_MIN
    results.append(("go (>= 1.22)", ok, f"{ver[0]}.{ver[1]}" if ok else "missing/old"))

    # git
    ok = bool(shutil.which("git"))
    results.append(("git", ok, shutil.which("git") or "missing"))

    # Built binaries
    for name, target in SYMLINKS.items():
        ok = target.exists()
        results.append((f"binary: {name}", ok, str(target) if ok else "not built"))

    # Symlinks
    for name in SYMLINKS:
        link = BIN_DIR / name
        ok = link.is_symlink()
        results.append((f"symlink: {name}", ok, str(link) if ok else "missing"))

    pad = max(len(r[0]) for r in results) + 2
    print()
    for label, ok, detail in results:
        status = f"{G}OK{W}" if ok else f"{R}FAIL{W}"
        print(f"  {label:<{pad}} {status}  {detail}")

    failed = [r for r in results if not r[1]]
    print()
    if not failed:
        info(f"{G}Mole installation complete and verified.{W}")
    else:
        warn(f"{Y}{len(failed)} item(s) need attention (see above).{W}")


# ─── Windows note ─────────────────────────────────────────────────────────────

def windows_note():
    print(f"""
{Y}[NOTE] Windows detected.{W}

  Mole must be built manually on Windows:
    1. Install Go 1.22+ from https://go.dev/dl/
    2. Install Git from https://git-scm.com/
    3. Clone the repo to a suitable directory.
    4. Open a Developer Command Prompt and run:
         go build ./...
       or follow the project README for platform-specific build instructions.
    5. Transparent TCP/DNS proxy features may require elevated privileges
       and Windows Filtering Platform (WFP) instead of iptables.
""")


# ─── main ─────────────────────────────────────────────────────────────────────

def parse_args():
    p = argparse.ArgumentParser(
        description=f"Installer for {APP_NAME}",
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument("--skip-apt",    "--skip-system", dest="skip_apt",
                   action="store_true", help="Skip system package installation")
    p.add_argument("--skip-pip",    action="store_true",
                   help="Skip pip package installation")
    p.add_argument("--skip-go",     action="store_true",
                   help="Skip Go module download step")
    p.add_argument("--skip-git",    action="store_true",
                   help="Skip build step")
    p.add_argument("--check",       action="store_true",
                   help="Check dependencies without installing")
    return p.parse_args()


def _check_prereqs():
    REQUIRED = [
        ("go",   "go — required to compile Mole (apt: golang / brew: go)"),
        ("git",  "git — required for cloning dependencies"),
        ("make", "make — required for building native components (apt: make)"),
        ("gcc",  "gcc — required for CGO compilation (apt: gcc)"),
    ]
    missing = []
    for binary, desc in REQUIRED:
        if not shutil.which(binary):
            missing.append(f"  ✗ {binary}  ({desc})")
    if missing:
        print("\nERROR: Missing required prerequisites:\n")
        for m in missing:
            print(m)
        print("\nInstall them first, then re-run install.py.\n")
        sys.exit(1)


def main():
    banner()
    args = parse_args()
    _check_prereqs()
    os_type = detect_os()
    info(f"Detected OS type: {os_type}")

    if args.check:
        ok = run_check()
        sys.exit(0 if ok else 1)

    if os_type == "windows":
        windows_note()
        sys.exit(0)

    require_root()

    # Phase 1 — system packages
    install_system_packages(os_type, args)

    # Go version guard (after package manager install)
    if not args.skip_apt:
        ensure_go_version(os_type)

    # Phase 2 — pip (none for Mole)
    install_pip_packages(args)

    # Phase 3 — go mod download
    install_go_deps(args)

    # Phase 4 — build
    build_app(args)

    # Symlinks
    install_symlinks()

    # Summary
    post_install_summary()


if __name__ == "__main__":
    main()
