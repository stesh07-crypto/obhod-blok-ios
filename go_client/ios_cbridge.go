//go:build ios

package main

/*
// These are C shim functions because CGO cannot call function pointer directly.
#include <stdlib.h>

typedef void (*LogCallbackFn)(const char* line, int isError);
typedef void (*StatsCallbackFn)(const char* stats);

static void bridge_log(LogCallbackFn fn, const char* line, int isError) {
    if (fn) fn(line, isError);
}

static void bridge_stats(StatsCallbackFn fn, const char* stats) {
    if (fn) fn(stats);
}
*/
import "C"
