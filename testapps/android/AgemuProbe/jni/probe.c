// Native crashes for the probe app's lab screen (LabActivity): a null write (SIGSEGV,
// SEGV_MAPERR) and abort() with a message (SIGABRT, "Abort message" in the tombstone).
#include <android/log.h>
#include <jni.h>
#include <stdlib.h>

__attribute__((noinline)) static void probe_null_write(volatile int *p) { *p = 42; }

JNIEXPORT void JNICALL Java_dev_agemu_probe_LabActivity_crashNative(JNIEnv *env, jclass cls) {
    (void)env;
    (void)cls;
    probe_null_write((volatile int *)0);
}

JNIEXPORT void JNICALL Java_dev_agemu_probe_LabActivity_abortNative(JNIEnv *env, jclass cls, jstring msg) {
    (void)cls;
    const char *m = (*env)->GetStringUTFChars(env, msg, NULL);
    // Sets the abort message (the tombstone's "Abort message: '...'") and aborts.
    __android_log_assert(NULL, "AgemuLab", "%s", m);
}
