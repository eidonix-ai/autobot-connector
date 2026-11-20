#include <time.h>
#include <string.h>
#include <inttypes.h>
#include <nvml.h>
#include <stdio.h>

#define MAX_GPUS 4

struct gpu {
    nvmlDevice_t handle;
    char uuid[NVML_DEVICE_UUID_V2_BUFFER_SIZE];
    const char *name;
    int minor;
    int xid_available;
    unsigned long long xid_count, xid;
};

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

static int identify(struct gpu *g, unsigned int index) {
    char name[NVML_DEVICE_NAME_V2_BUFFER_SIZE] = "";
    int major = 0;
    if (nvmlDeviceGetHandleByIndex_v2(index, &g->handle) != NVML_SUCCESS ||
        nvmlDeviceGetUUID(g->handle, g->uuid, sizeof(g->uuid)) != NVML_SUCCESS ||
        strncmp(g->uuid, "GPU-", 4) != 0 || strspn(g->uuid + 4, "0123456789abcdef-") != strlen(g->uuid + 4) ||
        nvmlDeviceGetName(g->handle, name, sizeof(name)) != NVML_SUCCESS ||
        nvmlDeviceGetCudaComputeCapability(g->handle, &major, &g->minor) != NVML_SUCCESS ||
        major != 12)
        return 0;
    if (g->minor == 1 && strcmp(name, "NVIDIA GB10") == 0)
        g->name = "NVIDIA GB10";
    else if (g->minor == 0 && strcmp(name, "NVIDIA RTX PRO 6000 Blackwell Max-Q Workstation Edition") == 0)
        g->name = "NVIDIA RTX PRO 6000 Blackwell Max-Q Workstation Edition";
    return g->name != NULL;
}

static void sample(const struct gpu *g) {
    nvmlDevice_t gpu = g->handle;
    printf("{\"uuid\":\"%s\",\"name\":\"%s\",\"sm\":\"12.%d\"", g->uuid, g->name, g->minor);
    nvmlTemperature_t reading_temperature = {
        .version = nvmlTemperature_v1, .sensorType = NVML_TEMPERATURE_GPU,
    };
    if (nvmlDeviceGetTemperatureV(gpu, &reading_temperature) == NVML_SUCCESS)
        printf(",\"temperature\":%d", reading_temperature.temperature);
    else
        printf(",\"temperature\":null");
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
    reading("memory_used_mib", rc, memory.used >> 20);
    reading("xid_count", g->xid_available ? NVML_SUCCESS : NVML_ERROR_NOT_SUPPORTED, g->xid_count);
    reading("last_xid", g->xid_available && g->xid_count ? NVML_SUCCESS : NVML_ERROR_NOT_SUPPORTED, g->xid);
    putchar('}');
}

int main(void) {
    struct gpu gpus[MAX_GPUS] = {0};
    unsigned int count = 0;
    if (nvmlInit_v2() != NVML_SUCCESS ||
        nvmlDeviceGetCount_v2(&count) != NVML_SUCCESS || count < 1 || count > MAX_GPUS)
        return 1;
    nvmlEventSet_t events;
    int event_set = nvmlEventSetCreate(&events) == NVML_SUCCESS;
    int any_events = 0;
    for (unsigned int i = 0; i < count; ++i) {
        struct gpu *g = &gpus[i];
        unsigned long long supported = 0;
        if (!identify(g, i))
            return 1;
        g->xid_available = event_set && nvmlDeviceGetSupportedEventTypes(g->handle, &supported) == NVML_SUCCESS &&
            (supported & nvmlEventTypeXidCriticalError) &&
            nvmlDeviceRegisterEvents(g->handle, nvmlEventTypeXidCriticalError, events) == NVML_SUCCESS;
        any_events |= g->xid_available;
    }
    for (;;) {
        putchar('[');
        for (unsigned int i = 0; i < count; ++i) {
            if (i)
                putchar(',');
            sample(&gpus[i]);
        }
        puts("]");
        fflush(stdout);
        if (any_events) {
            nvmlEventData_t event;
            nvmlReturn_t rc = nvmlEventSetWait_v2(events, &event, 500);
            if (rc == NVML_SUCCESS) {
                for (unsigned int i = 0; i < count; ++i) {
                    if (gpus[i].handle == event.device) {
                        ++gpus[i].xid_count;
                        gpus[i].xid = event.eventData;
                    }
                }
            } else if (rc != NVML_ERROR_TIMEOUT) {
                return 1;
            }
        } else {
            struct timespec delay = {0, 500000000};
            nanosleep(&delay, NULL);
        }
    }
}
