package assets

import (
	_ "embed"
)

//go:embed vmlinuz-virt
var Kernel []byte

//go:embed initramfs.cpio.gz
var Initramfs []byte
