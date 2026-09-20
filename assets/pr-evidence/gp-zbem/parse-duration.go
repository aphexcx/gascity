// Compare the spec's edge inputs with the local Go parser.
package main

import (
	"fmt"
	"time"
)

func main() {
	for _, s := range []string{"0", "-0", "+0", "00", "-00", "0.0", ".0", "0.", ".0h", "0.h", "0µs", "0μs", "0h0.0m", "0h0", "0h 0m", "."} {
		d, err := time.ParseDuration(s)
		fmt.Printf("%q: duration=%s error=%v\n", s, d, err)
	}
}
