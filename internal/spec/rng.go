package spec

import "math/bits"

// pyRandom is CPython's random.Random (MT19937) for integer seeds, enough to
// reproduce random.Random(seed).shuffle exactly.
type pyRandom struct {
	mt  [624]uint32
	idx int
}

func newPyRandom(seed int64) *pyRandom {
	r := &pyRandom{}
	// random_seed: the key is abs(seed) split into 32-bit words, least
	// significant first; zero seeds use the single word 0.
	n := uint64(seed)
	if seed < 0 {
		n = uint64(-seed)
	}
	key := []uint32{uint32(n)}
	if hi := uint32(n >> 32); hi != 0 {
		key = append(key, hi)
	}
	r.initByArray(key)
	return r
}

func (r *pyRandom) initGenrand(s uint32) {
	r.mt[0] = s
	for i := 1; i < 624; i++ {
		r.mt[i] = 1812433253*(r.mt[i-1]^(r.mt[i-1]>>30)) + uint32(i)
	}
	r.idx = 624
}

func (r *pyRandom) initByArray(key []uint32) {
	r.initGenrand(19650218)
	i, j := 1, 0
	k := 624
	if len(key) > k {
		k = len(key)
	}
	for ; k > 0; k-- {
		r.mt[i] = (r.mt[i] ^ ((r.mt[i-1] ^ (r.mt[i-1] >> 30)) * 1664525)) + key[j] + uint32(j)
		i++
		j++
		if i >= 624 {
			r.mt[0] = r.mt[623]
			i = 1
		}
		if j >= len(key) {
			j = 0
		}
	}
	for k = 623; k > 0; k-- {
		r.mt[i] = (r.mt[i] ^ ((r.mt[i-1] ^ (r.mt[i-1] >> 30)) * 1566083941)) - uint32(i)
		i++
		if i >= 624 {
			r.mt[0] = r.mt[623]
			i = 1
		}
	}
	r.mt[0] = 0x80000000
}

func (r *pyRandom) uint32() uint32 {
	const upper, lower = 0x80000000, 0x7fffffff
	mag := [2]uint32{0, 0x9908b0df}
	if r.idx >= 624 {
		var kk int
		for kk = 0; kk < 624-397; kk++ {
			y := (r.mt[kk] & upper) | (r.mt[kk+1] & lower)
			r.mt[kk] = r.mt[kk+397] ^ (y >> 1) ^ mag[y&1]
		}
		for ; kk < 623; kk++ {
			y := (r.mt[kk] & upper) | (r.mt[kk+1] & lower)
			r.mt[kk] = r.mt[kk+(397-624)] ^ (y >> 1) ^ mag[y&1]
		}
		y := (r.mt[623] & upper) | (r.mt[0] & lower)
		r.mt[623] = r.mt[396] ^ (y >> 1) ^ mag[y&1]
		r.idx = 0
	}
	y := r.mt[r.idx]
	r.idx++
	y ^= y >> 11
	y ^= (y << 7) & 0x9d2c5680
	y ^= (y << 15) & 0xefc60000
	y ^= y >> 18
	return y
}

// getrandbits for k <= 32 (enough for lists shorter than 2^32).
func (r *pyRandom) getrandbits(k int) uint32 {
	if k <= 0 {
		return 0
	}
	return r.uint32() >> (32 - k)
}

// randbelow is Random._randbelow_with_getrandbits.
func (r *pyRandom) randbelow(n int) int {
	k := bits.Len(uint(n))
	v := int(r.getrandbits(k))
	for v >= n {
		v = int(r.getrandbits(k))
	}
	return v
}

// shuffle is Random.shuffle: Fisher-Yates from the end.
func shuffle[T any](r *pyRandom, x []T) {
	for i := len(x) - 1; i > 0; i-- {
		j := r.randbelow(i + 1)
		x[i], x[j] = x[j], x[i]
	}
}
