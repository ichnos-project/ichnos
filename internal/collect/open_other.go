//go:build !unix

package collect

import "os"

const openFlags = os.O_RDONLY
