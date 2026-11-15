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

package cgroups

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const Root = "/sys/fs/cgroup"

func Ensure(name string) (string, error) {
	p := filepath.Join(Root, name)
	if err := os.MkdirAll(p, 0755); err != nil {
		return "", fmt.Errorf("create cgroup %s: %w", p, err)
	}
	return p, nil
}

func OpenFD(name string) (int, error) {
	p, err := Ensure(name)
	if err != nil {
		return -1, err
	}
	fd, err := syscall.Open(p, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open cgroup %s: %w", p, err)
	}
	return fd, nil
}

func EnableControllers(name string, controllers ...string) error {
	if _, err := Ensure(name); err != nil {
		return err
	}
	var b strings.Builder
	for _, c := range controllers {
		b.WriteString("+" + c + " ")
	}
	f := filepath.Join(Root, name, "cgroup.subtree_control")
	if err := os.WriteFile(f, []byte(strings.TrimSpace(b.String())), 0644); err != nil {
		return fmt.Errorf("enable controllers on %s: %w", name, err)
	}
	return nil
}
