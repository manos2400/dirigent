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

const (
	firecrackerMemoryOverheadMiB  = uint64(128)
	firecrackerMinimumMemoryMiB   = uint64(192)
	firecrackerMemoryAlignmentMiB = uint64(64)
)

func computeEffectiveVMMemoryMiB(requestedMemoryMiB uint64) uint64 {
	base := requestedMemoryMiB + firecrackerMemoryOverheadMiB
	if base < requestedMemoryMiB {
		base = ^uint64(0)
	}

	if base < firecrackerMinimumMemoryMiB {
		base = firecrackerMinimumMemoryMiB
	}

	return alignUpMiB(base, firecrackerMemoryAlignmentMiB)
}

func alignUpMiB(value uint64, alignment uint64) uint64 {
	if alignment == 0 {
		return value
	}

	remainder := value % alignment
	if remainder == 0 {
		return value
	}

	delta := alignment - remainder
	if value > ^uint64(0)-delta {
		return ^uint64(0)
	}

	return value + delta
}
