#!/usr/bin/env bash
set -euo pipefail

ARCH="$(uname -m)"
S3="https://s3.amazonaws.com/spec.ccfc.min"

echo "Detecting latest Firecracker CI artifacts..."

# Get the latest CI artifact directory
CI_ARTIFACTS_PREFIX=$(
    curl -fsSL "$S3?list-type=2&prefix=firecracker-ci/&delimiter=/" \
    | grep -oP '(?<=<Prefix>)firecracker-ci/[0-9]{8}-[^/]+/(?=</Prefix>)' \
    | sort \
    | tail -1
)

# Find the latest kernel for the current architecture
LATEST_KERNEL_KEY=$(
    curl -fsSL "$S3?list-type=2&prefix=${CI_ARTIFACTS_PREFIX}${ARCH}/vmlinux-" \
    | grep -oP "(?<=<Key>)(${CI_ARTIFACTS_PREFIX}${ARCH}/vmlinux-[0-9]+\.[0-9]+\.[0-9]{1,3})(?=</Key>)" \
    | sort -V \
    | tail -1
)

if [[ -z "$LATEST_KERNEL_KEY" ]]; then
    echo "Failed to locate latest kernel for architecture: $ARCH"
    exit 1
fi

echo "Downloading:"
echo "  $LATEST_KERNEL_KEY"

wget -c "$S3/$LATEST_KERNEL_KEY"

echo
echo "Downloaded kernel:"
basename "$LATEST_KERNEL_KEY"