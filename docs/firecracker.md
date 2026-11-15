Kernel compilation and image building:
- https://gruchalski.com/posts/2021-02-06-taking-firecracker-for-a-spin/
- https://github.com/firecracker-microvm/firecracker/blob/main/docs/rootfs-and-kernel-setup.md

### Quick Reference

**Build base rootfs template** (once, ~21 MB):
```bash
cd configs/firecracker/
sudo ./image.sh -r ./base-rootfs.ext4
```

**Build per-function rootfs** (once per function, off critical path):
```bash
sudo ./build_function_rootfs.sh \
    -b ./base-rootfs.ext4 \
    -i ghcr.io/manos2400/functionbench-matmul:latest \
    -o ./images/matmul.ext4
```

**Test a function VM** (standalone, no dirigent required):
```bash
sudo ./test_vm.sh \
    -r ./images/matmul.ext4 \
    -k ./vmlinux-5.10.245.bin \
    -p 80 -d
```

### Config Fields

| Field | Description |
|-------|-------------|
| `firecrackerKernel` | Path to the guest kernel binary |
| `firecrackerBaseRootfs` | Path to `base-rootfs.ext4` (Alpine + OpenRC + overlay-init) |
| `firecrackerImageStorePath` | Directory for per-function rootfs images |
| `firecrackerUseSnapshots` | Must be `false` (read-only rootfs incompatible with snapshots) |
| `firecrackerFileSystem` | **Deprecated** — kept for backward compatibility, not used by the runtime |