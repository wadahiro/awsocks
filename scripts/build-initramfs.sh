#!/bin/bash
# Build initramfs containing Go agent binary and kernel modules
# Requires: Docker with --privileged support

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "${SCRIPT_DIR}")"
OUTPUT_DIR="${OUTPUT_DIR:-${PROJECT_DIR}/internal/vm/assets}"

echo "==> Building Go agent..."

# Build Go agent as static binary for Linux
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags '-s -w -extldflags "-static"' \
    -o "${PROJECT_DIR}/tmp/agent" \
    "${PROJECT_DIR}/cmd/agent"

AGENT_SIZE=$(ls -lh "${PROJECT_DIR}/tmp/agent" | awk '{print $5}')
echo "Agent binary size: ${AGENT_SIZE}"

echo "==> Building initramfs using Docker..."

# Create initramfs inside Docker container
docker run --rm --privileged \
    -v "${PROJECT_DIR}/tmp:/build" \
    -v "${OUTPUT_DIR}:/output" \
    alpine:edge sh -c '
set -e

echo "[1/6] Installing kernel and modules package..."
apk add --no-cache linux-virt kmod >/dev/null 2>&1

KVER=$(ls /lib/modules | head -1)
echo "Kernel version: ${KVER}"

# Copy kernel to output (to ensure version match)
cp /boot/vmlinuz-virt /output/vmlinuz-virt
echo "Kernel copied to output"

echo "[2/6] Creating initramfs structure..."
mkdir -p /tmp/initramfs/dev
mkdir -p /tmp/initramfs/proc
mkdir -p /tmp/initramfs/sys
mkdir -p /tmp/initramfs/etc
mkdir -p /tmp/initramfs/tmp
mkdir -p /tmp/initramfs/sbin
mkdir -p /tmp/initramfs/lib/modules/${KVER}

echo "[3/6] Copying kernel modules for vsock and virtio-net..."
# vsock modules
cp /lib/modules/${KVER}/kernel/net/vmw_vsock/vsock.ko.gz /tmp/initramfs/lib/modules/${KVER}/
cp /lib/modules/${KVER}/kernel/net/vmw_vsock/vmw_vsock_virtio_transport_common.ko.gz /tmp/initramfs/lib/modules/${KVER}/
cp /lib/modules/${KVER}/kernel/net/vmw_vsock/vmw_vsock_virtio_transport.ko.gz /tmp/initramfs/lib/modules/${KVER}/
# virtio-net module and its dependencies (failover -> net_failover -> virtio_net)
cp /lib/modules/${KVER}/kernel/net/core/failover.ko.gz /tmp/initramfs/lib/modules/${KVER}/
cp /lib/modules/${KVER}/kernel/drivers/net/net_failover.ko.gz /tmp/initramfs/lib/modules/${KVER}/
cp /lib/modules/${KVER}/kernel/drivers/net/virtio_net.ko.gz /tmp/initramfs/lib/modules/${KVER}/
# Generate modules.dep
depmod -b /tmp/initramfs ${KVER}

echo "[4/6] Installing busybox and CA certificates..."
apk add --no-cache busybox-static ca-certificates >/dev/null 2>&1
cp /bin/busybox.static /tmp/initramfs/sbin/busybox

# Copy CA certificates for TLS verification (used by Go agent)
mkdir -p /tmp/initramfs/etc/ssl/certs
cp /etc/ssl/certs/ca-certificates.crt /tmp/initramfs/etc/ssl/certs/

echo "[5/6] Setting up busybox symlinks..."
ln -s busybox /tmp/initramfs/sbin/modprobe
ln -s busybox /tmp/initramfs/sbin/insmod
ln -s busybox /tmp/initramfs/sbin/uname
ln -s busybox /tmp/initramfs/sbin/sleep
ln -s busybox /tmp/initramfs/sbin/sh
ln -s busybox /tmp/initramfs/sbin/ifconfig
ln -s busybox /tmp/initramfs/sbin/udhcpc

# Create udhcpc script for DHCP configuration
cat > /tmp/initramfs/sbin/udhcpc-script << 'DHCPEOF'
#!/sbin/busybox sh
case "$1" in
    bound|renew)
        /sbin/busybox ifconfig $interface $ip netmask $subnet up
        if [ -n "$router" ]; then
            /sbin/busybox route add default gw $router dev $interface
        fi
        if [ -n "$dns" ]; then
            echo "nameserver $dns" > /etc/resolv.conf
        fi
        ;;
esac
DHCPEOF
chmod +x /tmp/initramfs/sbin/udhcpc-script

echo "[6/6] Creating init script..."
# Copy Go agent as /agent
cp /build/agent /tmp/initramfs/agent
chmod +x /tmp/initramfs/agent

# Create /init as shell script with kernel version embedded
cat > /tmp/initramfs/init << INITEOF
#!/sbin/busybox sh
# Load kernel modules before starting agent
/sbin/busybox mkdir -p /proc /sys /dev /etc /tmp
/sbin/busybox mount -t proc proc /proc
/sbin/busybox mount -t sysfs sysfs /sys
/sbin/busybox mount -t devtmpfs devtmpfs /dev
/sbin/busybox mount -t tmpfs tmpfs /tmp

MODDIR=/lib/modules/${KVER}

echo "[init] Kernel version: ${KVER}"
echo "[init] Loading vsock modules..."
/sbin/busybox gunzip -c \${MODDIR}/vsock.ko.gz > /tmp/vsock.ko 2>/dev/null && /sbin/insmod /tmp/vsock.ko || echo "[init] vsock load failed"
/sbin/busybox gunzip -c \${MODDIR}/vmw_vsock_virtio_transport_common.ko.gz > /tmp/vt_common.ko 2>/dev/null && /sbin/insmod /tmp/vt_common.ko || echo "[init] vsock_common load failed"
/sbin/busybox gunzip -c \${MODDIR}/vmw_vsock_virtio_transport.ko.gz > /tmp/vt.ko 2>/dev/null && /sbin/insmod /tmp/vt.ko || echo "[init] vsock_transport load failed"

echo "[init] Loading virtio_net module..."
/sbin/busybox gunzip -c \${MODDIR}/failover.ko.gz > /tmp/failover.ko 2>/dev/null && /sbin/insmod /tmp/failover.ko || echo "[init] failover load failed"
/sbin/busybox gunzip -c \${MODDIR}/net_failover.ko.gz > /tmp/net_failover.ko 2>/dev/null && /sbin/insmod /tmp/net_failover.ko || echo "[init] net_failover load failed"
/sbin/busybox gunzip -c \${MODDIR}/virtio_net.ko.gz > /tmp/virtio_net.ko 2>/dev/null && /sbin/insmod /tmp/virtio_net.ko || echo "[init] virtio_net load failed"

echo "[init] Waiting for network interface..."
/sbin/busybox sleep 1
/sbin/busybox ls /sys/class/net/ 2>/dev/null

# Configure loopback interface
echo "[init] Configuring loopback interface..."
/sbin/busybox ifconfig lo 127.0.0.1 netmask 255.0.0.0 up

# Configure network via DHCP
if [ -e /sys/class/net/eth0 ]; then
    echo "[init] Running DHCP on eth0..."
    /sbin/busybox ifconfig eth0 up
    /sbin/busybox udhcpc -i eth0 -s /sbin/udhcpc-script -n -q -t 5 -T 2
    DHCP_RESULT=\$?
    echo "[init] DHCP result: \${DHCP_RESULT}"
    /sbin/busybox ifconfig eth0 2>/dev/null
    /sbin/busybox route -n 2>/dev/null
    echo "[init] DNS config:"
    /sbin/busybox cat /etc/resolv.conf 2>/dev/null || echo "[init] No resolv.conf"
fi

echo "[init] Modules loaded, starting agent..."
# CA certificates for TLS (used by Go agent for HTTPS/WebSocket)
export SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
export SSL_CERT_DIR=/etc/ssl/certs

# Start the Go agent
exec /agent
INITEOF
chmod +x /tmp/initramfs/init

echo "[*] Creating cpio archive..."
cd /tmp/initramfs
find . -print0 | cpio --null -o --format=newc 2>/dev/null | gzip -9 > /output/initramfs.cpio.gz

echo "Done!"
ls -lh /output/initramfs.cpio.gz
'

# Cleanup
rm -f "${PROJECT_DIR}/tmp/agent"

echo "==> initramfs ready at ${OUTPUT_DIR}/initramfs.cpio.gz"
