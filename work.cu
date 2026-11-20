#include <cuda_runtime.h>
#include <cstdio>
#include <unistd.h>

static void check(cudaError_t result) {
    if (result != cudaSuccess) {
        std::fprintf(stderr, "%s\n", cudaGetErrorString(result));
        std::exit(1);
    }
}

__global__ void touch(unsigned *memory, size_t words) {
    for (size_t i = blockIdx.x * blockDim.x + threadIdx.x; i < words;
         i += gridDim.x * blockDim.x)
        memory[i] = static_cast<unsigned>(i) ^ 0x121;
}

int main() {
    constexpr size_t bytes = 2ull << 30;
    cudaDeviceProp device{};
    check(cudaGetDeviceProperties(&device, 0));
    unsigned *memory;
    check(cudaMalloc(&memory, bytes));
    touch<<<512, 256>>>(memory, bytes / sizeof(*memory));
    check(cudaGetLastError());
    check(cudaDeviceSynchronize());
    unsigned sample = 0;
    check(cudaMemcpy(&sample, memory + 4096, sizeof(sample), cudaMemcpyDeviceToHost));
    if (sample != (4096 ^ 0x121) || device.major != 12 || device.minor != 1 || getuid() != 1000)
        return 1;
    std::printf("SM%d%d uid=%d pid=%d touched_bytes=%zu sample=%u\n",
                device.major, device.minor, getuid(), getpid(), bytes, sample);
    std::fflush(stdout);
    sleep(60);
    check(cudaFree(memory));
}
