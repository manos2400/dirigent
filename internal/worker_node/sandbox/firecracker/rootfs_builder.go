/*
 * MIT License
 *
 * Copyright (c) 2024 EASL
 *
 * Permission is hereby granted, free of charge, to any person obtaining a copy
 * of this software and associated documentation files (the "Software"), to deal
 * in the Software without restriction, including without limitation the rights
 * to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
 * copies of the Software, and to permit persons to whom the Software is
 * furnished to do so, subject to the following conditions:
 *
 * The above copyright notice and this permission notice shall be included in all
 * copies or substantial portions of the Software.
 *
 * THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
 * IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
 * FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
 * AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
 * LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
 * OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
 * SOFTWARE.
 */

package firecracker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"
)

// FunctionRootfs holds metadata about a built per-function rootfs image.
type FunctionRootfs struct {
	ImageName  string
	RootfsPath string
}

type RootfsBuilder struct {
	launcherPath   string
	imageStorePath string

	cache sync.Map   // imageName → *FunctionRootfs
	mu    sync.Mutex // serialise concurrent builds for the same image
}

type ociConfig struct {
	Entrypoint []string `json:"Entrypoint"`
	Cmd        []string `json:"Cmd"`
	Env        []string `json:"Env"`
	WorkingDir string   `json:"WorkingDir"`
	User       string   `json:"User"`
}

type ociInspect struct {
	Config ociConfig `json:"Config"`
}

type fcEntrypoint struct {
	Cmd     []string `json:"cmd"`
	Workdir string   `json:"workdir"`
	Env     []string `json:"env"`
}

func NewRootfsBuilder(launcherPath, imageStorePath string) (*RootfsBuilder, error) {
	if _, err := os.Stat(launcherPath); err != nil {
		return nil, fmt.Errorf("fc-launcher binary not found at %s: %w", launcherPath, err)
	}
	if err := os.MkdirAll(imageStorePath, 0755); err != nil {
		return nil, fmt.Errorf("cannot create image store directory %s: %w", imageStorePath, err)
	}

	return &RootfsBuilder{
		launcherPath:   launcherPath,
		imageStorePath: imageStorePath,
	}, nil
}

func (rb *RootfsBuilder) GetOrBuild(imageName string) (*FunctionRootfs, error) {
	// cache hit
	if v, ok := rb.cache.Load(imageName); ok {
		return v.(*FunctionRootfs), nil
	}

	// serialise builds so we don't build the same image twice.
	rb.mu.Lock()
	defer rb.mu.Unlock()

	// Double-check after acquiring lock
	if v, ok := rb.cache.Load(imageName); ok {
		return v.(*FunctionRootfs), nil
	}

	rootfs, err := rb.build(imageName)
	if err != nil {
		return nil, err
	}

	rb.cache.Store(imageName, rootfs)
	return rootfs, nil
}

func sanitizeImageName(imageName string) string {
	r := strings.NewReplacer("/", "_", ":", "_", "@", "_")
	return r.Replace(imageName)
}

func (rb *RootfsBuilder) build(imageName string) (*FunctionRootfs, error) {
	outputPath := filepath.Join(rb.imageStorePath, sanitizeImageName(imageName)+".ext4")

	// Reuse existing rootfs from a previous run if present on disk.
	if info, err := os.Stat(outputPath); err == nil && info.Size() > 0 {
		logrus.Infof("[rootfs-builder] Reusing existing rootfs for %s → %s", imageName, outputPath)
		return &FunctionRootfs{
			ImageName:  imageName,
			RootfsPath: outputPath,
		}, nil
	}

	logrus.Infof("[rootfs-builder] Building OCI-native rootfs for %s → %s", imageName, outputPath)

	// Step 1: Pull the OCI image
	if err := runCmd("docker", "pull", imageName); err != nil {
		return nil, fmt.Errorf("docker pull failed for %s: %w", imageName, err)
	}

	// Step 2: Export OCI filesystem
	ociExportDir, err := os.MkdirTemp("", "fc-oci-export-")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(ociExportDir)

	containerID, err := runCmdOutput("docker", "create", imageName)
	if err != nil {
		return nil, fmt.Errorf("docker create failed: %w", err)
	}
	containerID = strings.TrimSpace(containerID)
	defer runCmd("docker", "rm", containerID)

	exportCmd := exec.Command("docker", "export", containerID)
	tarCmd := exec.Command("tar", "x", "-C", ociExportDir)
	tarCmd.Stdin, err = exportCmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to pipe docker export: %w", err)
	}
	if err := tarCmd.Start(); err != nil {
		return nil, fmt.Errorf("tar start failed: %w", err)
	}
	if err := exportCmd.Run(); err != nil {
		return nil, fmt.Errorf("docker export failed: %w", err)
	}
	if err := tarCmd.Wait(); err != nil {
		return nil, fmt.Errorf("tar extract failed: %w", err)
	}

	// Clean up Docker artifacts that shouldn't be in the rootfs
	os.Remove(filepath.Join(ociExportDir, ".dockerenv"))

	// Step 3: Extract entrypoint from image config
	inspectJSON, err := runCmdOutput("docker", "inspect", imageName)
	if err != nil {
		return nil, fmt.Errorf("docker inspect failed: %w", err)
	}

	var inspectResult []ociInspect
	if err := json.Unmarshal([]byte(inspectJSON), &inspectResult); err != nil {
		return nil, fmt.Errorf("failed to parse docker inspect output: %w", err)
	}
	if len(inspectResult) == 0 {
		return nil, fmt.Errorf("docker inspect returned empty result for %s", imageName)
	}

	cfg := inspectResult[0].Config

	// Build final command: Entrypoint + Cmd (Docker semantics)
	var finalCmd []string
	if len(cfg.Entrypoint) > 0 {
		finalCmd = append(finalCmd, cfg.Entrypoint...)
		finalCmd = append(finalCmd, cfg.Cmd...)
	} else {
		finalCmd = cfg.Cmd
	}

	if len(finalCmd) == 0 {
		return nil, fmt.Errorf("image %s has no Entrypoint or Cmd — cannot determine what to run", imageName)
	}

	workdir := cfg.WorkingDir
	if workdir == "" {
		workdir = "/"
	}

	entrypoint := fcEntrypoint{
		Cmd:     finalCmd,
		Workdir: workdir,
		Env:     cfg.Env,
	}

	logrus.Infof("[rootfs-builder]   Image entrypoint: %v, workdir: %s", finalCmd, workdir)

	// Step 4: Inject boot artifacts into OCI export

	// /sbin/fc-launcher (static binary: overlay init + PID-1 launcher)
	sbinDir := filepath.Join(ociExportDir, "sbin")
	os.MkdirAll(sbinDir, 0755)
	if err := copyFile(rb.launcherPath, filepath.Join(sbinDir, "fc-launcher")); err != nil {
		return nil, fmt.Errorf("failed to inject fc-launcher: %w", err)
	}
	os.Chmod(filepath.Join(sbinDir, "fc-launcher"), 0755)

	// /.fc-entrypoint.json (metadata read by fc-launcher at boot)
	entrypointJSON, err := json.MarshalIndent(entrypoint, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal entrypoint: %w", err)
	}
	if err := os.WriteFile(filepath.Join(ociExportDir, ".fc-entrypoint.json"), entrypointJSON, 0644); err != nil {
		return nil, fmt.Errorf("failed to write .fc-entrypoint.json: %w", err)
	}

	// Ensure essential directories exist
	for _, dir := range []string{"mnt", "proc", "sys", "dev", "tmp", "run", "var"} {
		os.MkdirAll(filepath.Join(ociExportDir, dir), 0755)
	}
	// /tmp must be world-writable
	os.Chmod(filepath.Join(ociExportDir, "tmp"), 01777)

	// Step 5: Determine rootfs size
	ociSizeBytes, ociFileCount, err := dirSizeAndCount(ociExportDir)
	if err != nil {
		return nil, fmt.Errorf("failed to measure OCI export dir: %w", err)
	}

	const (
		baseMB        = 64   // ext4 journal + superblocks
		overheadRatio = 0.30 // 30% for block bitmaps, directory overhead, fragmentation
		bytesPerInode = 4096 // conservative: ext4 default is 16384 but small files need more
		minSizeMB     = 256  // never go below this
	)
	inodeOverhead := ociFileCount * bytesPerInode
	contentOverhead := int64(float64(ociSizeBytes) * overheadRatio)
	neededBytes := ociSizeBytes + contentOverhead + int64(inodeOverhead) + baseMB*1024*1024
	sizeMB := neededBytes / (1024 * 1024)
	if sizeMB < minSizeMB {
		sizeMB = minSizeMB
	}

	logrus.Infof("[rootfs-builder]   OCI content: %d MB (%d files), function rootfs: %d MB",
		ociSizeBytes/(1024*1024), ociFileCount, sizeMB)

	// Step 6: Create ext4 image and populate
	if err := runCmd("truncate", "-s", fmt.Sprintf("%dM", sizeMB), outputPath); err != nil {
		return nil, fmt.Errorf("truncate failed: %w", err)
	}
	if err := runCmd("mkfs.ext4", "-F", outputPath); err != nil {
		os.Remove(outputPath)
		return nil, fmt.Errorf("mkfs.ext4 failed: %w", err)
	}

	mountDir, err := os.MkdirTemp("", "fc-rootfs-mount-")
	if err != nil {
		os.Remove(outputPath)
		return nil, fmt.Errorf("failed to create mount dir: %w", err)
	}
	defer func() {
		runCmd("umount", mountDir)
		os.RemoveAll(mountDir)
	}()

	if err := runCmd("mount", "-o", "loop", outputPath, mountDir); err != nil {
		os.Remove(outputPath)
		return nil, fmt.Errorf("mount failed: %w", err)
	}

	if err := runCmd("cp", "-a", ociExportDir+"/.", mountDir+"/"); err != nil {
		os.Remove(outputPath)
		return nil, fmt.Errorf("failed to copy OCI content into rootfs: %w", err)
	}

	// Unmount
	if err := runCmd("umount", mountDir); err != nil {
		return nil, fmt.Errorf("failed to unmount rootfs: %w", err)
	}

	logrus.Infof("[rootfs-builder]   Function rootfs built: %s", outputPath)

	return &FunctionRootfs{
		ImageName:  imageName,
		RootfsPath: outputPath,
	}, nil
}

//  Helpers

func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %v failed: %w (stderr: %s)", name, args, err, stderr.String())
	}
	return nil
}

func runCmdOutput(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %v failed: %w (stderr: %s)", name, args, err, stderr.String())
	}
	return stdout.String(), nil
}

func copyFile(src, dst string) error {
	return runCmd("cp", src, dst)
}

func dirSizeAndCount(path string) (totalBytes int64, fileCount int64, err error) {
	err = filepath.Walk(path, func(_ string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		fileCount++
		if !info.IsDir() {
			totalBytes += info.Size()
		}
		return nil
	})
	return totalBytes, fileCount, err
}
