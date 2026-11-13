package sandbox

import "errors"

func openTTY() (uint16, uint16, <-chan [2]uint16, func(), error) {
	return 0, 0, nil, nil, errors.New("sandboxed terminals are not supported on Windows")
}
