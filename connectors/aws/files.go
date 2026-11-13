package aws

import (
	"os"

	"github.com/muthuishere/tmvp/internal/secure"
)

const pathSep = os.PathSeparator

func writeEmpty(p string) error { return secure.WriteFile(p, nil) }
