package relay

import (
	"crypto/ed25519"
	"errors"
	"math/big"
	"slices"
)

// The curve behind Ed25519 keys is -x² + y² = 1 + d·x²·y² over the integers
// modulo the prime p = 2²⁵⁵ - 19, with d = -121665/121666.
var (
	curveP = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))
	curveD = func() *big.Int {
		d := new(big.Int).ModInverse(big.NewInt(121666), curveP)
		d.Mul(d, big.NewInt(-121665))
		return d.Mod(d, curveP)
	}()
)

var (
	errKeyLength       = errors.New("not 32 bytes")
	errKeyNotCanonical = errors.New("not the canonical encoding of a point")
	errKeyOffCurve     = errors.New("not a point on the curve")
	errKeySmallOrder   = errors.New("a point of small order")
)

// checkSigningKey admits an Ed25519 public key only if it is the canonical
// encoding of a point on the curve (RFC 8032, section 5.1.3) and the point is
// not of small order. A signature can verify under a point of small order
// without anybody's private key, so such a key proves nothing about who
// signed. Every source of the relay's signing keys checks its keys here.
func checkSigningKey(key []byte) error {
	if len(key) != ed25519.PublicKeySize {
		return errKeyLength
	}
	// The encoding is y, least significant byte first, with the low bit of x
	// in its top bit.
	b := slices.Clone(key)
	slices.Reverse(b)
	odd := b[0]&0x80 != 0
	b[0] &= 0x7f
	y := new(big.Int).SetBytes(b)
	if y.Cmp(curveP) >= 0 {
		return errKeyNotCanonical
	}
	// x² = (y² - 1) / (d·y² + 1); the divisor is never zero on this curve.
	yy := fieldMul(y, y)
	u := fieldMod(new(big.Int).Sub(yy, big.NewInt(1)))
	v := fieldMod(new(big.Int).Add(fieldMul(curveD, yy), big.NewInt(1)))
	vInverse := new(big.Int).ModInverse(v, curveP)
	if vInverse == nil {
		return errKeyOffCurve
	}
	x := new(big.Int).ModSqrt(fieldMul(u, vInverse), curveP)
	switch {
	case x == nil:
		return errKeyOffCurve
	case x.Sign() == 0 && odd:
		return errKeyNotCanonical
	case (x.Bit(0) == 1) != odd:
		x.Sub(curveP, x)
	}
	if hasSmallOrder(x, y) {
		return errKeySmallOrder
	}
	return nil
}

// hasSmallOrder reports whether eight times the point (x, y) is the identity
// point (0, 1): the points of small order are the curve's subgroup of order
// 8. It doubles three times in projective coordinates (X:Y:Z) by the doubling
// formula for a = -1 (dbl-2008-bbjlp), which holds for every point on the
// curve.
func hasSmallOrder(x, y *big.Int) bool {
	X, Y, Z := x, y, big.NewInt(1)
	for range 3 {
		xx, yy, zz := fieldMul(X, X), fieldMul(Y, Y), fieldMul(Z, Z)
		f := fieldMod(new(big.Int).Sub(yy, xx))
		j := fieldMod(new(big.Int).Sub(f, new(big.Int).Lsh(zz, 1)))
		X = fieldMul(new(big.Int).Lsh(fieldMul(X, Y), 1), j)
		Y = fieldMul(new(big.Int).Neg(new(big.Int).Add(xx, yy)), f)
		Z = fieldMul(f, j)
	}
	return X.Sign() == 0 && Z.Sign() != 0 && Y.Cmp(Z) == 0
}

func fieldMod(z *big.Int) *big.Int { return z.Mod(z, curveP) }

func fieldMul(a, b *big.Int) *big.Int { return fieldMod(new(big.Int).Mul(a, b)) }
