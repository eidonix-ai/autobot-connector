#include <time.h>
#include <string.h>
#include <inttypes.h>
#include <nvml.h>
#include <stdio.h>

static void reading(const char *key, nvmlReturn_t rc, unsigned long long value) {
    printf(",\"%s\":", key);
    if (rc == NVML_SUCCESS)
        printf("%llu", value);
    else
        printf("null");
}

#define READ(key, query) do { \
    unsigned int value = 0; \
    nvmlReturn_t rc = (query); \
    reading(key, rc, value); \
} while (0)

int main(void) {
    nvmlDevice_t gpu;
    unsigned int devices = 0;
    if (nvmlInit_v2() != NVML_SUCCESS ||
        nvmlDeviceGetCount_v2(&devices) != NVML_SUCCESS || devices != 1 ||
        nvmlDeviceGetHandleByIndex_v2(0, &gpu) != NVML_SUCCESS)
        return 1;
    char name[NVML_DEVICE_NAME_V2_BUFFER_SIZE] = "";
    int major = 0, minor = 0;
    if (nvmlDeviceGetName(gpu, name, sizeof(name)) != NVML_SUCCESS ||
        nvmlDeviceGetCudaComputeCapability(gpu, &major, &minor) != NVML_SUCCESS)
        return 1;
    // This profile supports only the GB10. Emit a fixed string as JSON.
    if (strcmp(name, "NVIDIA GB10") != 0 || major != 12 || minor != 1)
        return 1;
    nvmlEventSet_t events;
    unsigned long long supported = 0, xid_count = 0, xid = 0;
    int xid_available = nvmlDeviceGetSupportedEventTypes(gpu, &supported) == NVML_SUCCESS &&
        (supported & nvmlEventTypeXidCriticalError) &&
        nvmlEventSetCreate(&events) == NVML_SUCCESS &&
        nvmlDeviceRegisterEvents(gpu, nvmlEventTypeXidCriticalError, events) == NVML_SUCCESS;
    for (;;) {
        printf("{\"name\":\"NVIDIA GB10\",\"sm\":\"12.1\"");
        READ("temperature", nvmlDeviceGetTemperature(gpu, NVML_TEMPERATURE_GPU, &value));
        READ("power_mw", nvmlDeviceGetPowerUsage(gpu, &value));
        READ("power_limit_mw", nvmlDeviceGetPowerManagementLimit(gpu, &value));
        READ("graphics_mhz", nvmlDeviceGetClockInfo(gpu, NVML_CLOCK_GRAPHICS, &value));
        READ("memory_mhz", nvmlDeviceGetClockInfo(gpu, NVML_CLOCK_MEM, &value));
        READ("fan", nvmlDeviceGetFanSpeed(gpu, &value));
        unsigned long long flags = 0, corrected = 0, uncorrected = 0;
        nvmlReturn_t rc = nvmlDeviceGetCurrentClocksEventReasons(gpu, &flags);
        reading("throttle", rc, flags);
        rc = nvmlDeviceGetTotalEccErrors(gpu, NVML_MEMORY_ERROR_TYPE_CORRECTED, NVML_VOLATILE_ECC, &corrected);
        if (rc == NVML_SUCCESS)
            rc = nvmlDeviceGetTotalEccErrors(gpu, NVML_MEMORY_ERROR_TYPE_UNCORRECTED, NVML_VOLATILE_ECC, &uncorrected);
        reading("ecc", rc, corrected + uncorrected);
        nvmlMemory_t memory = {0};
        rc = nvmlDeviceGetMemoryInfo(gpu, &memory);
        reading("memory_mib", rc, memory.total >> 20);
        reading("xid_count", xid_available ? NVML_SUCCESS : NVML_ERROR_NOT_SUPPORTED, xid_count);
        reading("last_xid", xid_available && xid_count ? NVML_SUCCESS : NVML_ERROR_NOT_SUPPORTED, xid);
        puts("}");
        fflush(stdout);
        if (xid_available) {
            nvmlEventData_t event;
            rc = nvmlEventSetWait_v2(events, &event, 500);
            if (rc == NVML_SUCCESS) {
                ++xid_count;
                xid = event.eventData;
            } else if (rc != NVML_ERROR_TIMEOUT) {
                return 1;
            }
        } else {
            struct timespec delay = {0, 500000000};
            nanosleep(&delay, NULL);
        }
    }
}
