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
	"cluster_manager/internal/worker_node/managers"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/sirupsen/logrus"
)

// maxSnapshotAttempts caps how often one key is retried. Every attempt pauses a live
// guest and dumps its memory, so a key that keeps failing must stop being retried on
// the cold start path instead of taxing every subsequent cold start.
const maxSnapshotAttempts = 3

type SnapshotManager struct {
	managers.RootFsManager[*SnapshotMetadata]
	creating sync.Map
	failures sync.Map // snapshotKey -> *atomic.Uint32
}

type SnapshotMetadata struct {
	MemoryPath   string
	SnapshotPath string
}

func NewFirecrackerSnapshotManager() *SnapshotManager {
	return &SnapshotManager{}
}

func SnapshotKey(image string, effectiveMemoryMiB uint64, requestedCPUCores uint64, guestPort int32) string {
	return fmt.Sprintf("%s|mem=%d|cpu=%d|port=%d", image, effectiveMemoryMiB, requestedCPUCores, guestPort)
}

func (sm *SnapshotManager) Exists(snapshotKey string) bool {
	_, ok := sm.Get(snapshotKey)

	return ok
}

func (sm *SnapshotManager) AddSnapshot(snapshotKey string, metadata *SnapshotMetadata) {
	sm.LoadOrStore(snapshotKey, metadata)
}

func (sm *SnapshotManager) TryClaimCreation(snapshotKey string) bool {
	if sm.Exists(snapshotKey) || sm.exhausted(snapshotKey) {
		return false
	}

	if _, alreadyCreating := sm.creating.LoadOrStore(snapshotKey, struct{}{}); alreadyCreating {
		return false
	}

	if sm.Exists(snapshotKey) {
		sm.creating.Delete(snapshotKey)
		return false
	}

	return true
}

func (sm *SnapshotManager) ReleaseCreation(snapshotKey string) {
	sm.creating.Delete(snapshotKey)
}

func (sm *SnapshotManager) RecordFailure(snapshotKey string) {
	counter, _ := sm.failures.LoadOrStore(snapshotKey, &atomic.Uint32{})

	if attempts := counter.(*atomic.Uint32).Add(1); attempts >= maxSnapshotAttempts {
		logrus.Warnf("Giving up on snapshotting %s after %d failed attempts.", snapshotKey, attempts)
	}
}

func (sm *SnapshotManager) exhausted(snapshotKey string) bool {
	counter, ok := sm.failures.Load(snapshotKey)

	return ok && counter.(*atomic.Uint32).Load() >= maxSnapshotAttempts
}

func (sm *SnapshotManager) FindSnapshot(snapshotKey string) (*SnapshotMetadata, bool) {
	return sm.Get(snapshotKey)
}
