#!/bin/bash
#
# MIT License
#
# Copyright (c) 2024 EASL
#
# Permission is hereby granted, free of charge, to any person obtaining a copy
# of this software and associated documentation files (the "Software"), to deal
# in the Software without restriction, including without limitation the rights
# to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
# copies of the Software, and to permit persons to whom the Software is
# furnished to do so, subject to the following conditions:
#
# The above copyright notice and this permission notice shall be included in all
# copies or substantial portions of the Software.
#
# THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
# IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
# FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
# AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
# LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
# OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
# SOFTWARE.
#
# build_function_rootfs.sh — Creates a per-function rootfs for Firecracker VMs.
#
#
# The per-function rootfs is used READ-ONLY by Firecracker VMs. A tmpfs
# overlay inside the guest (set up by overlay-init) provides writability.
#
# Usage:
#   sudo ./build_function_rootfs.sh -i <oci-image> [-l <fc-launcher>] [-o <output-path>] [-s <size-mb>]
#

set -e

OCI_IMAGE=""
OUTPUT_PATH=""
SIZE_MB=""
LAUNCHER_PATH=""

while getopts "i:o:s:l:" optname; do
    case "${optname}" in
        i)  OCI_IMAGE=${OPTARG} ;;
        o)  OUTPUT_PATH=${OPTARG} ;;
        s)  SIZE_MB=${OPTARG} ;;
        l)  LAUNCHER_PATH=${OPTARG} ;;
        *)  echo "Unknown option ${optname}"; exit 1 ;;
    esac
done

if [ -z "$OCI_IMAGE" ]; then
    echo "Error: -i <oci-image> is required"
    echo "Usage: sudo $0 -i <oci-image> [-l <fc-launcher>] [-o <output-path>] [-s <size-mb>]"
    exit 1
fi

SCRIPT_DIR=$(dirname "$(realpath "${BASH_SOURCE}")")

# Auto-detect fc-launcher binary
if [ -z "$LAUNCHER_PATH" ]; then
    # Look in script directory, then project root
    for candidate in \
        "$SCRIPT_DIR/fc-launcher" \
        "$SCRIPT_DIR/../../fc-launcher" \
        "$(which fc-launcher 2>/dev/null)"; do
        if [ -n "$candidate" ] && [ -x "$candidate" ]; then
            LAUNCHER_PATH="$candidate"
            break
        fi
    done
fi

if [ -z "$LAUNCHER_PATH" ] || [ ! -x "$LAUNCHER_PATH" ]; then
    echo "Error: fc-launcher binary not found or not executable."
    echo "Build it first with:"
    echo "  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o fc-launcher -trimpath -ldflags='-s -w' ./cmd/fc_launcher"
    echo "Or specify it with: -l <path-to-fc-launcher>"
    exit 1
fi

IMAGE_SANITIZED=$(echo "$OCI_IMAGE" | sed 's|[/:@]|_|g')

if [ -z "$OUTPUT_PATH" ]; then
    OUTPUT_PATH="$(pwd)/images/${IMAGE_SANITIZED}.ext4"
fi

MOUNT_DIR=""
OCI_EXPORT_DIR=""
CONTAINER_ID=""

cleanup() {
    set +e
    if [ -n "$MOUNT_DIR" ] && mountpoint -q "$MOUNT_DIR" 2>/dev/null; then
        umount "$MOUNT_DIR"
    fi
    [ -n "$MOUNT_DIR" ] && rm -rf "$MOUNT_DIR"
    [ -n "$OCI_EXPORT_DIR" ] && rm -rf "$OCI_EXPORT_DIR"
    [ -n "$CONTAINER_ID" ] && docker rm "$CONTAINER_ID" >/dev/null 2>&1
}
trap cleanup EXIT

# Step 1: Pull the OCI image
echo ">>> Pulling OCI image: $OCI_IMAGE"
docker pull "$OCI_IMAGE"

# Step 2: Extract OCI image filesystem
echo ">>> Extracting image filesystem..."
OCI_EXPORT_DIR=$(mktemp -d /tmp/fc-oci-export-XXXXXX)
CONTAINER_ID=$(docker create "$OCI_IMAGE")
docker export "$CONTAINER_ID" | tar x -C "$OCI_EXPORT_DIR"
rm -f "$OCI_EXPORT_DIR/.dockerenv"

# Step 3: Extract entrypoint/cmd from OCI config
echo ">>> Extracting entrypoint from image config..."
OCI_CONFIG=$(docker inspect "$OCI_IMAGE")

ENTRYPOINT_JSON=$(echo "$OCI_CONFIG" | jq -r '.[0].Config.Entrypoint // empty')
CMD_JSON=$(echo "$OCI_CONFIG" | jq -r '.[0].Config.Cmd // empty')
WORKDIR=$(echo "$OCI_CONFIG" | jq -r '.[0].Config.WorkingDir // empty')
ENV_JSON=$(echo "$OCI_CONFIG" | jq -r '.[0].Config.Env // empty')

# Build the final command: Entrypoint + Cmd (Docker semantics)
if [ -n "$ENTRYPOINT_JSON" ] && [ "$ENTRYPOINT_JSON" != "null" ]; then
    if [ -n "$CMD_JSON" ] && [ "$CMD_JSON" != "null" ]; then
        FINAL_CMD=$(jq -n --argjson ep "$ENTRYPOINT_JSON" --argjson cmd "$CMD_JSON" '$ep + $cmd')
    else
        FINAL_CMD="$ENTRYPOINT_JSON"
    fi
elif [ -n "$CMD_JSON" ] && [ "$CMD_JSON" != "null" ]; then
    FINAL_CMD="$CMD_JSON"
else
    echo "Error: No Entrypoint or Cmd found in image config — cannot determine what to run."
    exit 1
fi

# Build /.fc-entrypoint.json
FC_ENTRYPOINT=$(jq -n \
    --argjson cmd "$FINAL_CMD" \
    --arg workdir "${WORKDIR:-/}" \
    --argjson env "${ENV_JSON:-[]}" \
    '{cmd: $cmd, workdir: $workdir, env: $env}')

echo "   Entrypoint: $FC_ENTRYPOINT"

# Step 4: Inject boot artifacts into OCI export
echo ">>> Injecting boot artifacts..."

# /sbin/fc-launcher (static binary: overlay init + PID-1 launcher)
mkdir -p "$OCI_EXPORT_DIR/sbin"
cp "$LAUNCHER_PATH" "$OCI_EXPORT_DIR/sbin/fc-launcher"
chmod +x "$OCI_EXPORT_DIR/sbin/fc-launcher"

# /.fc-entrypoint.json (metadata read by fc-launcher at boot)
echo "$FC_ENTRYPOINT" > "$OCI_EXPORT_DIR/.fc-entrypoint.json"

# Ensure essential directories exist (some minimal images may lack them)
for dir in mnt proc sys dev tmp run var; do
    mkdir -p "$OCI_EXPORT_DIR/$dir"
done
chmod 1777 "$OCI_EXPORT_DIR/tmp"

# Step 5: Determine rootfs size
# Measure actual content size and file count for inode overhead estimation
OCI_SIZE_BYTES=$(du -sb "$OCI_EXPORT_DIR" | cut -f1)
OCI_FILE_COUNT=$(find "$OCI_EXPORT_DIR" | wc -l)

if [ -z "$SIZE_MB" ]; then
    BASE_OVERHEAD=$((64 * 1024 * 1024))   # 64 MB for journal + superblocks
    CONTENT_OVERHEAD=$((OCI_SIZE_BYTES * 30 / 100))  # 30% for bitmaps, dir overhead, fragmentation
    INODE_OVERHEAD=$((OCI_FILE_COUNT * 4096))         # 4 KB per file/dir for inode overhead
    NEEDED_BYTES=$((OCI_SIZE_BYTES + CONTENT_OVERHEAD + INODE_OVERHEAD + BASE_OVERHEAD))
    SIZE_MB=$(( (NEEDED_BYTES / 1024 / 1024) + 1 ))
    if [ "$SIZE_MB" -lt 256 ]; then
        SIZE_MB=256
    fi
fi

echo ">>> OCI content: $(( OCI_SIZE_BYTES / 1024 / 1024 )) MB ($OCI_FILE_COUNT files)"
echo ">>> Function rootfs size: ${SIZE_MB} MB"

# Step 6: Create ext4 image and populate
mkdir -p "$(dirname "$OUTPUT_PATH")"
echo ">>> Creating function rootfs: $OUTPUT_PATH"

truncate -s "${SIZE_MB}M" "$OUTPUT_PATH"
mkfs.ext4 -F "$OUTPUT_PATH"

MOUNT_DIR=$(mktemp -d /tmp/fc-rootfs-mount-XXXXXX)
mount -o loop "$OUTPUT_PATH" "$MOUNT_DIR"

echo ">>> Populating rootfs with OCI image content + boot artifacts..."
cp -a "$OCI_EXPORT_DIR"/. "$MOUNT_DIR"/

# Unmount
umount "$MOUNT_DIR"
rm -rf "$MOUNT_DIR"
MOUNT_DIR=""

# Fix ownership
if [ -n "$SUDO_UID" ]; then
    chown "$SUDO_UID" "$OUTPUT_PATH"
fi

echo ""
echo "============================================"
echo " Function rootfs built: $OUTPUT_PATH"
echo " Size: $(du -h "$OUTPUT_PATH" | cut -f1)"
echo " Image: $OCI_IMAGE"
echo " Command: $(echo "$FINAL_CMD" | jq -r 'join(" ")')"
echo " Mode: OCI-native (no Alpine base, no OpenRC)"
echo ""
echo " Boot with Firecracker using:"
echo "   Drive: $OUTPUT_PATH (IsReadOnly=true)"
echo "   Kernel args: root=/dev/vda ro init=/sbin/overlay-init ..."
echo "============================================"
