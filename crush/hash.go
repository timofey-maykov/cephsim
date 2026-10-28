package crush

// rjenkins1 из ceph/src/crush/hash.c.

const hashSeed = 1315423911

func hashmix(a, b, c uint32) (uint32, uint32, uint32) {
	a = a - b
	a = a - c
	a = a ^ (c >> 13)
	b = b - c
	b = b - a
	b = b ^ (a << 8)
	c = c - a
	c = c - b
	c = c ^ (b >> 13)
	a = a - b
	a = a - c
	a = a ^ (c >> 12)
	b = b - c
	b = b - a
	b = b ^ (a << 16)
	c = c - a
	c = c - b
	c = c ^ (b >> 5)
	a = a - b
	a = a - c
	a = a ^ (c >> 3)
	b = b - c
	b = b - a
	b = b ^ (a << 10)
	c = c - a
	c = c - b
	c = c ^ (b >> 15)
	return a, b, c
}

// Hash32_2 - crush_hash32_rjenkins1_2, в том числе для pps пула.
func Hash32_2(a, b uint32) uint32 { return hash32_2(a, b) }

func hash32_2(a, b uint32) uint32 {
	hash := hashSeed ^ a ^ b
	x, y := uint32(231232), uint32(1232)
	a, b, hash = hashmix(a, b, hash)
	x, a, hash = hashmix(x, a, hash)
	b, y, hash = hashmix(b, y, hash)
	return hash
}

func hash32_3(a, b, c uint32) uint32 {
	hash := hashSeed ^ a ^ b ^ c
	x, y := uint32(231232), uint32(1232)
	a, b, hash = hashmix(a, b, hash)
	c, x, hash = hashmix(c, x, hash)
	y, a, hash = hashmix(y, a, hash)
	b, x, hash = hashmix(b, x, hash)
	y, c, hash = hashmix(y, c, hash)
	return hash
}
