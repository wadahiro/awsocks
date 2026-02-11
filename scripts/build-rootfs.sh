#!/bin/bash
# Build Alpine Linux rootfs image using Docker
# Requires: Docker with --privileged support (for loop mount)
set -e

ALPINE_VERSION="${ALPINE_VERSION:-3.19.1}"
OUTPUT_DIR="${OUTPUT_DIR:-internal/vm/assets}"
ROOTFS_SIZE=256

# Check if Docker is running
if ! docker info >/dev/null 2>&1; then
    echo "Error: Docker is not running"
    echo "Please start Docker first"
    exit 1
fi

echo "==> Building rootfs using Docker..."

# Build rootfs inside Docker container
docker run --rm --privileged -v "$(pwd)/${OUTPUT_DIR}:/output" alpine:${ALPINE_VERSION} sh -c "
set -e

ROOTFS_SIZE=${ROOTFS_SIZE}
ALPINE_VERSION=${ALPINE_VERSION}

apk add --no-cache e2fsprogs curl >/dev/null 2>&1

echo '[1/6] Creating disk image...'
dd if=/dev/zero of=/tmp/rootfs.img bs=1M count=\${ROOTFS_SIZE} 2>/dev/null

echo '[2/6] Formatting ext4...'
mkfs.ext4 -F /tmp/rootfs.img >/dev/null 2>&1

echo '[3/6] Mounting and extracting Alpine...'
mkdir -p /tmp/mnt
mount -o loop /tmp/rootfs.img /tmp/mnt
curl -sL \"https://dl-cdn.alpinelinux.org/alpine/v3.19/releases/x86_64/alpine-minirootfs-\${ALPINE_VERSION}-x86_64.tar.gz\" | tar xz -C /tmp/mnt

echo '[4/6] Configuring system...'
echo 'nameserver 8.8.8.8' > /tmp/mnt/etc/resolv.conf
echo 'alpine-vm' > /tmp/mnt/etc/hostname
echo '127.0.0.1 localhost' > /tmp/mnt/etc/hosts

cat > /tmp/mnt/etc/inittab << 'EOF'
::sysinit:/bin/mount -o remount,rw /
::sysinit:/bin/mount -t proc proc /proc
::sysinit:/bin/mount -t devpts devpts /dev/pts
::sysinit:/sbin/ifconfig lo 127.0.0.1 up
::sysinit:/sbin/ifconfig eth0 192.168.64.10 up
::sysinit:/sbin/route add default gw 192.168.64.1
hvc0::respawn:/sbin/getty -n -l /bin/sh 0 hvc0 dumb
::shutdown:/bin/umount -a
EOF

sed -i 's/root:\*:/root::/' /tmp/mnt/etc/shadow

cat > /tmp/mnt/etc/profile << 'EOF'
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PS1=\"alpine# \"
export TERM=dumb
export ENV=/root/.ashrc
EOF

cat > /tmp/mnt/root/.ashrc << 'EOF'
export TERM=dumb
EOF

echo '[5/6] Unmounting...'
umount /tmp/mnt

echo '[6/6] Compressing...'
gzip -9 -f /tmp/rootfs.img
mv /tmp/rootfs.img.gz /output/

echo 'Done!'
ls -lh /output/rootfs.img.gz
"

echo "==> rootfs.img.gz ready at ${OUTPUT_DIR}/"
