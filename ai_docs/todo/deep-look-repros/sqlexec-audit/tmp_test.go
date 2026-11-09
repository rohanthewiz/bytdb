package sqlexecaudit

import "os"

// scratchTmp is a process-lifetime temp dir for the shared benchmark db.
var scratchTmp = func() string {
	d, err := os.MkdirTemp("", "sqlexecaudit")
	if err != nil {
		panic(err)
	}
	return d
}()
