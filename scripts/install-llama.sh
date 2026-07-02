#!/usr/bin/env bash
set -e

echo "======================================================"
echo "    llama.cpp Unified Installer                        "
echo "    Targets: 8845HS/780M, Intel Core Ultra + Blackwell"
echo "======================================================"

# ------------------------------------------------------------
# 1. Inputs
# ------------------------------------------------------------
echo "Select your hardware target:"
echo "1) AOOSTAR GEM12 - AMD 8845HS + Radeon 780M (integrated iGPU only — NOT for eGPU!)   [Vulkan]"
echo "2) AOOSTAR GEM12 - AMD 8845HS + NVIDIA 5060 Ti eGPU (OCuLink AG03 / Thunderbolt)   [CUDA]"
echo "3) Intel Core Ultra + NVIDIA RTX 500 Pro 6GB (dedicated)   [CUDA]"
echo "4) Intel Core Ultra + NVIDIA 5060 Ti eGPU (Thunderbolt 5)   [CUDA]"
echo "5) Vulkan (Universal fallback — may pick wrong GPU on multi-GPU systems)"
echo "6) CPU only"
read -rp "Choice [1-6]: " HW_CHOICE

REPO_URL="https://github.com/ggml-org/llama.cpp.git"
BRANCH_NAME="master"

# ------------------------------------------------------------
# 2. Cleanup Old Binaries & Services
# ------------------------------------------------------------
echo ""
echo "Cleaning up..."
sudo systemctl stop llama-server.service 2>/dev/null || true
sudo pkill -f llama-server 2>/dev/null || true
sudo rm -f /usr/local/bin/llama-* /usr/local/lib/libggml* /usr/local/lib/libllama* 2>/dev/null || true

# ------------------------------------------------------------
# 3. Base Dependencies
# ------------------------------------------------------------
echo "Installing base build tools..."
sudo apt-get update -y
sudo apt-get install -y build-essential cmake git curl wget pkg-config ninja-build

# ------------------------------------------------------------
# 4. Setup CMake Arguments
# ------------------------------------------------------------
# -march=native picks up Zen 4 AVX-512 (8845HS) or Intel Core Ultra AVX2 + VNNI
CMAKE_ARGS=(
    "-DBUILD_SHARED_LIBS=OFF"
    "-DGGML_BACKEND_DL=OFF"
    "-DLLAMA_BUILD_EXAMPLES=OFF"
    "-DLLAMA_BUILD_TESTS=OFF"
    "-DCMAKE_BUILD_TYPE=Release"
    "-G" "Ninja"
    "-DCMAKE_C_FLAGS=-march=native"
    "-DCMAKE_CXX_FLAGS=-march=native"
)

BACKEND=""
case "$HW_CHOICE" in
    1)
        echo "Target: AOOSTAR GEM12 (8845HS) + Radeon 780M iGPU -> Vulkan"
        BACKEND="vulkan"
        ;;
    2)
        echo "Target: AOOSTAR GEM12 (8845HS) + NVIDIA 5060 Ti eGPU (OCuLink/TB) -> CUDA"
        BACKEND="cuda"
        ;;
    3)
        echo "Target: Intel Core Ultra + NVIDIA RTX 500 Pro -> CUDA"
        BACKEND="cuda"
        ;;
    4)
        echo "Target: Intel Core Ultra + NVIDIA 5060 Ti eGPU (TB5) -> CUDA"
        BACKEND="cuda"
        ;;
    5)
        echo "Target: Universal Vulkan fallback"
        BACKEND="vulkan"
        ;;
    6)
        echo "Target: CPU only"
        BACKEND="cpu"
        ;;
    *)
        echo "Invalid choice. Exiting."; exit 1;;
esac

case "$BACKEND" in
    cuda)
        echo "Configuring CUDA (sm_120 / Blackwell)..."

        if ! nvcc --version &>/dev/null; then
            echo " -> Installing CUDA 12 toolkit..."
            wget -q --no-netrc https://developer.download.nvidia.com/compute/cuda/repos/ubuntu2404/x86_64/cuda-keyring_1.1-1_all.deb -O /tmp/cuda-keyring.deb
            sudo dpkg -i /tmp/cuda-keyring.deb
            sudo apt-get update -y
            sudo apt-get install -y cuda-toolkit-12
            rm -f /tmp/cuda-keyring.deb
        fi

        if [[ -x /usr/local/cuda-12/bin/nvcc ]]; then
            CMAKE_ARGS+=("-DCMAKE_CUDA_COMPILER=/usr/local/cuda-12/bin/nvcc")
        elif [[ -x /usr/local/cuda/bin/nvcc ]]; then
            CMAKE_ARGS+=("-DCMAKE_CUDA_COMPILER=/usr/local/cuda/bin/nvcc")
        fi

        # Explicitly disable Vulkan to prevent the binary from picking up the
        # AMD iGPU via Vulkan when a CUDA build is intended.  Without this,
        # CMake auto-detects Vulkan (mesa-vulkan-drivers) and enables both
        # backends — at runtime Vulkan grabs the first GPU (iGPU) instead of
        # the NVIDIA eGPU.
        # All described targets are Blackwell (sm_120); no need to compile
        # Ada (sm_89) kernels — doing so only increases build time.
        CMAKE_ARGS+=("-DGGML_CUDA=ON" "-DGGML_VULKAN=OFF" "-DCMAKE_CUDA_ARCHITECTURES=120")
        CMAKE_ARGS+=("-DCMAKE_CUDA_FLAGS=-Wno-unused-parameter")
        ;;
    vulkan)
        echo "Configuring Vulkan..."
        sudo apt-get install -y libvulkan-dev vulkan-tools glslang-tools
        CMAKE_ARGS+=("-DGGML_VULKAN=ON")
        ;;
    cpu)
        echo "Configuring CPU..."
        ;;
esac

# ------------------------------------------------------------
# 5. Clone and Configure
# ------------------------------------------------------------
echo ""
echo "Downloading branch: $BRANCH_NAME..."
BUILD_DIR="$HOME/llama_build"
rm -rf "$BUILD_DIR"
git clone --depth 1 -b "$BRANCH_NAME" "$REPO_URL" "$BUILD_DIR"
cd "$BUILD_DIR"

echo "Running CMake Configuration..."
cmake -B build "${CMAKE_ARGS[@]}"

# ------------------------------------------------------------
# 6. ANTI-SILENT-FALLBACK CHECK
# ------------------------------------------------------------
echo ""
echo "Verifying CMake did not drop the selected backend..."
if [ "$BACKEND" == "cuda" ] && ! grep -q "GGML_CUDA:BOOL=ON" build/CMakeCache.txt; then
    echo "CRITICAL: CMake silently disabled CUDA because a dependency is missing. Stopping."
    exit 1
fi
if [ "$BACKEND" == "cuda" ] && grep -q "GGML_VULKAN:BOOL=ON" build/CMakeCache.txt; then
    echo "CRITICAL: CMake enabled Vulkan alongside CUDA — this will cause the binary to use the wrong GPU. Stopping."
    exit 1
fi
if [ "$BACKEND" == "vulkan" ] && ! grep -q "GGML_VULKAN:BOOL=ON" build/CMakeCache.txt; then
    echo "CRITICAL: CMake silently disabled Vulkan because a dependency is missing. Stopping."
    exit 1
fi
echo "Hardware backend confirmed active!"

# ------------------------------------------------------------
# 7. Build and Install
# ------------------------------------------------------------
echo ""
echo "Compiling using $(nproc) threads..."
cmake --build build -j"$(nproc)"

echo ""
echo "Installing to /usr/local/bin..."
sudo cmake --install build --prefix /usr/local
sudo ldconfig

# ------------------------------------------------------------
# 8. POST-INSTALL BINARY VERIFICATION
# ------------------------------------------------------------
echo ""
echo "Verifying installed binary..."

# Confirm the correct backend symbol is present and the wrong one is absent.
HAS_CUDA=0
HAS_VULKAN=0
if strings /usr/local/bin/llama-server 2>/dev/null | grep -q 'ggml_cuda_init'; then
    HAS_CUDA=1
fi
if strings /usr/local/bin/llama-server 2>/dev/null | grep -q 'ggml_vk_init'; then
    HAS_VULKAN=1
fi

case "$BACKEND" in
    cuda)
        if [ "$HAS_CUDA" -eq 1 ] && [ "$HAS_VULKAN" -eq 0 ]; then
            echo " -> OK: binary has CUDA backend and no Vulkan backend."
        elif [ "$HAS_CUDA" -eq 0 ]; then
            echo " -> CRITICAL: ggml_cuda_init not found in binary."
            echo "    The binary has no CUDA support — it will fall back to CPU/Vulkan."
            echo "    Ensure 'nvcc' is in PATH and CUDA 12 toolkit is installed, then re-run."
            exit 1
        else
            echo " -> CRITICAL: binary has BOTH CUDA and Vulkan backends."
            echo "    At runtime Vulkan will capture the AMD iGPU instead of the NVIDIA GPU."
            echo "    Rebuild with -DGGML_VULKAN=OFF (already set by this script — check CMake output)."
            exit 1
        fi
        ;;
    vulkan)
        if [ "$HAS_VULKAN" -eq 1 ]; then
            echo " -> OK: binary has Vulkan backend."
        else
            echo " -> WARNING: ggml_vk_init not found in binary — Vulkan may be missing."
        fi
        ;;
    cpu)
        echo " -> OK: CPU-only build."
        ;;
esac

echo "======================================================"
echo " Installation Complete! "
echo "======================================================"
if [ "$BACKEND" == "cuda" ]; then
    echo " CUDA backend: verify the GPU is visible (nvidia-smi)."
    echo " For eGPU, ensure the enclosure is connected before launching llama-server."
    echo " The binary has Vulkan disabled — it will always use the NVIDIA GPU exclusively."
    echo " No CUDA_VISIBLE_DEVICES prefix is needed in config.yaml."
fi
