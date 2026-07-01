#!/bin/bash
set -euo pipefail

# ==============================================================================
# Dell Pro Max 14 MC14250 (Intel Core Ultra 7 265H / Arrow Lake-H iGPU)
# NVIDIA RTX PRO 500 Blackwell Laptop GPU (sm_120)
# + optional NVIDIA 5060 Ti eGPU via OCuLink AG03 / Thunderbolt
# Ubuntu 26.04 LLM Host Optimization
# ==============================================================================

if [[ $EUID -ne 0 ]]; then
    echo "Run as root:"
    echo "sudo $0"
    exit 1
fi

PRIMARY_USER="$(id -nu 1000 2>/dev/null || true)"

echo
echo "========================================================="
echo " Dell Pro Max 14 LLM Optimization"
echo " Intel Core Ultra 7 265H + RTX PRO 500 + NVIDIA eGPU"
echo "========================================================="
echo

# ------------------------------------------------------------------------------
# Helper
# ------------------------------------------------------------------------------
add_grub_param() {
    local param="$1"
    if ! grep -q "$param" /etc/default/grub; then
        sed -i \
          "s/^GRUB_CMDLINE_LINUX_DEFAULT=\"\(.*\)\"/GRUB_CMDLINE_LINUX_DEFAULT=\"\1 $param\"/" \
          /etc/default/grub
    fi
}

remove_grub_param_regex() {
    local regex="$1"
    sed -i -E "s/${regex}//g" /etc/default/grub
}

enroll_thunderbolt_egpu() {
    command -v boltctl >/dev/null 2>&1 || return 0

    local raw
    raw="$(boltctl list 2>/dev/null | awk '
        /^[[:space:]]*●/ {
            if (uuid && stored == "no") print uuid "|" name
            name=""; uuid=""; stored=""
        }
        /[[:space:]]name:[[:space:]]+/ { sub(/.*name:[[:space:]]+/, ""); name=$0 }
        /[[:space:]]uuid:[[:space:]]+/ { sub(/.*uuid:[[:space:]]+/, ""); uuid=$0 }
        /[[:space:]]stored:[[:space:]]+/ { sub(/.*stored:[[:space:]]+/, ""); stored=$0 }
        END { if (uuid && stored == "no") print uuid "|" name }
    ')"

    [[ -z "$raw" ]] && { echo " -> No unenrolled Thunderbolt devices found."; return 0; }

    local uuids=() names=()
    while IFS='|' read -r u n; do
        [[ -z "$u" ]] && continue
        uuids+=("$u")
        names+=("${n:-unknown}")
    done <<< "$raw"

    echo " -> Found ${#uuids[@]} unenrolled Thunderbolt device(s):"
    for i in "${!uuids[@]}"; do
        echo "    [$((i+1))] ${names[$i]} (uuid: ${uuids[$i]})"
    done
    echo
    read -rp "Enroll which? [1-${#uuids[@]}, 'a'=all, Enter=skip]: " choice

    [[ -z "$choice" ]] && { echo " -> Skipping Thunderbolt enrollment."; return 0; }

    local to_enroll=()
    if [[ "$choice" =~ ^[aA]$ ]]; then
        to_enroll=("${uuids[@]}")
    elif [[ "$choice" =~ ^[0-9]+$ ]] && (( choice >= 1 && choice <= ${#uuids[@]} )); then
        to_enroll=("${uuids[$((choice-1))]}")
    else
        echo " -> Invalid choice. Skipping."
        return 0
    fi

    for u in "${to_enroll[@]}"; do
        echo " -> Enrolling $u (auto policy)..."
        boltctl enroll "$u" --policy auto || echo " -> WARNING: enrollment failed for $u"
    done
}

# ------------------------------------------------------------------------------
# 1. Swappiness
# ------------------------------------------------------------------------------
echo "[1/7] Configuring memory behaviour..."

cat >/etc/sysctl.d/99-llm.conf <<EOF
vm.swappiness=1
EOF

sysctl --system >/dev/null

echo " -> Swappiness set to 1"

# ------------------------------------------------------------------------------
# 2. GRUB configuration
# ------------------------------------------------------------------------------
echo "[2/7] Configuring GRUB..."

cp -n /etc/default/grub /etc/default/grub.backup

# Remove pcie_aspm=off — can destabilize Thunderbolt PCIe tunneling
remove_grub_param_regex 'pcie_aspm=off'

# Useful for eGPU / PCIe / Thunderbolt devices
add_grub_param "intel_iommu=on"
add_grub_param "iommu=pt"

# Disable PCIe AER
add_grub_param "pci=noaer"

# NVIDIA DRM modesetting required for Wayland + NVIDIA GPU compositing.
# Without this the compositor falls back to CPU/iGPU rendering -> laggy UI.
add_grub_param "nvidia-drm.modeset=1"

# pci=realloc / assign-busses / hpbussize break WiFi (M.2 PCIe) BAR mapping.
remove_grub_param_regex 'pci=realloc[^"]*'
remove_grub_param_regex 'assign-busses[^"]*'
remove_grub_param_regex 'hpbussize=[^"]*'

echo " -> GRUB parameters updated"

# ------------------------------------------------------------------------------
# 3. Early i915 + Thunderbolt loading
# ------------------------------------------------------------------------------
echo "[3/7] Configuring Intel iGPU + Thunderbolt loading..."

MODULES_FILE="/etc/initramfs-tools/modules"
grep -qxF "i915" "$MODULES_FILE" || echo "i915" >> "$MODULES_FILE"
grep -qxF "thunderbolt" "$MODULES_FILE" || echo "thunderbolt" >> "$MODULES_FILE"

echo " -> i915 + thunderbolt added to initramfs"

# ------------------------------------------------------------------------------
# 4. Useful packages (Moved up to ensure headers exist for DKMS)
# ------------------------------------------------------------------------------
echo "[4/7] Installing tooling and kernel headers..."

apt-get update
# linux-headers-generic covers the standard case; try the exact running
# kernel's headers too, but don't fail the whole script if they're absent
# (e.g. OEM/custom kernel without a matching -headers package).
apt-get install -y dkms linux-headers-generic || true
apt-get install -y "linux-headers-$(uname -r)" 2>/dev/null || true
apt-get install -y \
    nvtop \
    htop \
    lm-sensors \
    pciutils \
    vulkan-tools \
    mesa-vulkan-drivers \
    mesa-utils \
    intel-gpu-tools \
    bolt \
    linux-tools-common \
    linux-tools-generic

echo " -> Tooling installed"

# Remove stale VK_ICD_FILENAMES
if grep -q 'VK_ICD_FILENAMES' /etc/environment 2>/dev/null; then
    sed -i '/VK_ICD_FILENAMES/d' /etc/environment
    echo " -> Removed stale VK_ICD_FILENAMES from /etc/environment"
fi

# ------------------------------------------------------------------------------
# 5. NVIDIA preparation (RTX PRO 500 Blackwell + 5060 Ti eGPU)
# ------------------------------------------------------------------------------
echo "[5/7] Preparing NVIDIA environment (RTX PRO 500 + eGPU)..."

cat >/etc/modprobe.d/blacklist-nouveau.conf <<EOF
blacklist nouveau
options nouveau modeset=0
EOF

# Allow open kernel modules to bind to eGPU bridges (CRITICAL for 5060 Ti Blackwell)
cat >/etc/modprobe.d/nvidia-egpu.conf <<EOF
options nvidia NVreg_OpenRmEnableUnsupportedGpus=1
EOF

if ! command -v nvidia-smi >/dev/null 2>&1; then
    echo " -> NVIDIA driver not detected, installing open kernel modules..."
    # Blackwell (RTX 50 / PRO 500) needs the open kernel module variant.
    # 610.x is the latest production branch (replaces 595.x).
    apt-get install -y nvidia-driver-open || ubuntu-drivers autoinstall || true
else
    echo " -> NVIDIA driver already installed. Upgrading to latest open kernel modules..."
    apt-get install -y nvidia-driver-open || true
fi

# Enable persistence daemon
if command -v nvidia-persistenced >/dev/null 2>&1; then
    systemctl enable --now nvidia-persistenced 2>/dev/null || true
    nvidia-smi -pm 1 2>/dev/null || true
    echo " -> NVIDIA persistence enabled"
fi

# CUDA 13+ toolkit required for native sm_120 (Blackwell) llama.cpp builds.
# CUDA 13 supports GCC 15 natively and fixes the glibc 2.41 math conflict.
echo " -> Installing CUDA 13 toolkit for Blackwell (sm_120) support..."
if ! command -v nvcc >/dev/null 2>&1; then
    wget -q --no-netrc https://developer.download.nvidia.com/compute/cuda/repos/ubuntu2404/x86_64/cuda-keyring_1.1-1_all.deb -O /tmp/cuda-keyring.deb
    dpkg -i /tmp/cuda-keyring.deb
    apt-get update
    apt-get install -y cuda-toolkit-13
    rm -f /tmp/cuda-keyring.deb
    echo " -> CUDA 13 toolkit installed"
else
    echo " -> CUDA 13+ toolkit already present"
fi

# Interactively enroll Thunderbolt eGPU enclosure if connected
enroll_thunderbolt_egpu

# ------------------------------------------------------------------------------
# 6. User permissions
# ------------------------------------------------------------------------------
echo "[6/7] Configuring permissions..."

if [[ -n "${PRIMARY_USER}" ]]; then
    usermod -aG render,video "${PRIMARY_USER}"
    echo " -> Added ${PRIMARY_USER} to render/video groups"
else
    echo " -> User UID 1000 not found"
fi

# ------------------------------------------------------------------------------
# 7. Rebuild boot environment
# ------------------------------------------------------------------------------
echo "[7/7] Updating boot files..."

update-initramfs -u
update-grub

echo
echo "========================================================="
echo " Completed successfully."
echo
echo " Recommended checks after reboot:"
echo
echo "   lsmod | grep -E 'i915|thunderbolt|nvidia'"
echo "   lspci | grep -E 'VGA|3D|NVIDIA'"
echo "   vulkaninfo --summary"
echo "   nvidia-smi"
echo "   boltctl            # check TB eGPU enrollment"
echo "   nvtop"
echo
echo " Then rebuild llama.cpp with native sm_120 support:"
echo "   cd ~/llama_build/build && cmake .. -DGGML_CUDA=ON -DCMAKE_CUDA_ARCHITECTURES='89;90;120' -G Ninja && cmake --build . -j && sudo cmake --install . --prefix /usr/local"
echo "   strings /usr/local/bin/llama-server | grep '^sm_' | sort -u  # must show sm_120"
echo
echo " For TB eGPU: if GPU not visible, enroll the enclosure:"
echo "   sudo boltctl enroll <uuid> --policy auto"
echo
echo " Reboot required."
echo "========================================================="
echo
