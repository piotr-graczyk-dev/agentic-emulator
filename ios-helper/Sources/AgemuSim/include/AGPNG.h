#pragma once
#include <stddef.h>
#include <stdint.h>

/// Encodes a 32-bit little-endian BGRX image (alpha ignored) as an 8-bit RGB PNG at `path`,
/// compressing horizontal bands in parallel. `level` is the zlib level (1 = fastest).
/// Returns the file size in bytes, or -1 on failure.
long AGWritePNG(const uint8_t *bgrx, size_t width, size_t height, size_t bytesPerRow, const char *path, int level);
