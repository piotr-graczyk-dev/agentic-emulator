// A fast, parallel PNG encoder for 32-bit BGRX framebuffers.
//
// ImageIO's PNG encoder is single-threaded and spends ~90 ms on a 1206x2622 home screen. This
// one splits the image into horizontal bands, filters and deflates each band on its own core
// (zlib level 1, raw deflate, ended with Z_SYNC_FLUSH so the bands concatenate into one valid
// zlib stream, the same technique pigz uses), and combines the Adler-32 checksums.
// Output: 8-bit RGB (no alpha), sRGB chunk, one IDAT per band.
#include "AGPNG.h"

#include <dispatch/dispatch.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <zlib.h>

typedef struct {
  const uint8_t *bgrx;
  size_t width;
  size_t bytesPerRow;
  size_t rowStart;
  size_t rowEnd;
  int last;
  int level;
  uint8_t *out;
  size_t outLength;
  uLong adler;
  size_t rawLength;
  int failed;
} AGBand;

// Filter type 1 ("Sub"): each byte minus the byte one pixel to the left. Cheap, and it turns
// flat colour and horizontal gradients into runs of zeros.
static void AGFilterRow(const uint8_t *src, size_t width, uint8_t *dst)
{
  dst[0] = 1;
  uint8_t *p = dst + 1;
  uint8_t pr = 0, pg = 0, pb = 0;
  for (size_t x = 0; x < width; x++) {
    uint8_t b = src[4 * x + 0], g = src[4 * x + 1], r = src[4 * x + 2];
    p[0] = (uint8_t)(r - pr);
    p[1] = (uint8_t)(g - pg);
    p[2] = (uint8_t)(b - pb);
    pr = r;
    pg = g;
    pb = b;
    p += 3;
  }
}

static void AGDeflateBand(AGBand *band)
{
  size_t rowLength = 1 + band->width * 3;
  size_t rows = band->rowEnd - band->rowStart;
  band->rawLength = rows * rowLength;
  uint8_t *raw = malloc(band->rawLength);
  if (!raw) {
    band->failed = 1;
    return;
  }
  for (size_t y = 0; y < rows; y++) {
    AGFilterRow(band->bgrx + (band->rowStart + y) * band->bytesPerRow, band->width, raw + y * rowLength);
  }
  band->adler = adler32(adler32(0L, Z_NULL, 0), raw, (uInt)band->rawLength);

  z_stream z;
  memset(&z, 0, sizeof(z));
  if (deflateInit2(&z, band->level, Z_DEFLATED, -15, 8, Z_DEFAULT_STRATEGY) != Z_OK) {
    free(raw);
    band->failed = 1;
    return;
  }
  size_t bound = deflateBound(&z, (uLong)band->rawLength) + 64;
  band->out = malloc(bound);
  z.next_in = raw;
  z.avail_in = (uInt)band->rawLength;
  z.next_out = band->out;
  z.avail_out = (uInt)bound;
  int rc = deflate(&z, band->last ? Z_FINISH : Z_SYNC_FLUSH);
  if ((band->last && rc != Z_STREAM_END) || (!band->last && rc != Z_OK) || z.avail_in != 0) {
    band->failed = 1;
  }
  band->outLength = bound - z.avail_out;
  deflateEnd(&z);
  free(raw);
}

static void AGPut32(uint8_t *p, uint32_t v)
{
  p[0] = (uint8_t)(v >> 24);
  p[1] = (uint8_t)(v >> 16);
  p[2] = (uint8_t)(v >> 8);
  p[3] = (uint8_t)v;
}

static int AGWriteChunk(FILE *f, const char *type, const uint8_t *data, size_t length)
{
  uint8_t header[8];
  AGPut32(header, (uint32_t)length);
  memcpy(header + 4, type, 4);
  uLong crc = crc32(0L, Z_NULL, 0);
  crc = crc32(crc, (const Bytef *)type, 4);
  if (length) {
    crc = crc32(crc, data, (uInt)length);
  }
  uint8_t trailer[4];
  AGPut32(trailer, (uint32_t)crc);
  if (fwrite(header, 1, 8, f) != 8) {
    return 0;
  }
  if (length && fwrite(data, 1, length, f) != length) {
    return 0;
  }
  return fwrite(trailer, 1, 4, f) == 4;
}

long AGWritePNG(const uint8_t *bgrx, size_t width, size_t height, size_t bytesPerRow, const char *path, int level)
{
  if (!bgrx || width == 0 || height == 0) {
    return -1;
  }
  size_t bandCount = 12;
  if (bandCount > height) {
    bandCount = height;
  }
  AGBand *bands = calloc(bandCount, sizeof(AGBand));
  size_t rowsPerBand = (height + bandCount - 1) / bandCount;
  for (size_t i = 0; i < bandCount; i++) {
    bands[i].bgrx = bgrx;
    bands[i].width = width;
    bands[i].bytesPerRow = bytesPerRow;
    bands[i].rowStart = i * rowsPerBand;
    bands[i].rowEnd = (i + 1) * rowsPerBand < height ? (i + 1) * rowsPerBand : height;
    bands[i].level = level;
  }
  // Drop empty trailing bands (rounding) and mark the real last one.
  while (bandCount > 1 && bands[bandCount - 1].rowStart >= height) {
    bandCount--;
  }
  bands[bandCount - 1].last = 1;
  dispatch_apply(bandCount, DISPATCH_APPLY_AUTO, ^(size_t i) {
    AGDeflateBand(&bands[i]);
  });

  long written = -1;
  FILE *f = NULL;
  uLong adler = adler32(0L, Z_NULL, 0);
  for (size_t i = 0; i < bandCount; i++) {
    if (bands[i].failed) {
      goto done;
    }
    adler = adler32_combine(adler, bands[i].adler, (z_off_t)bands[i].rawLength);
  }
  f = fopen(path, "wb");
  if (!f) {
    goto done;
  }
  static const uint8_t signature[8] = {0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'};
  fwrite(signature, 1, 8, f);
  uint8_t ihdr[13];
  AGPut32(ihdr, (uint32_t)width);
  AGPut32(ihdr + 4, (uint32_t)height);
  ihdr[8] = 8;   // bit depth
  ihdr[9] = 2;   // colour type: RGB
  ihdr[10] = 0;  // deflate
  ihdr[11] = 0;  // adaptive filtering (per-row filter byte)
  ihdr[12] = 0;  // no interlace
  if (!AGWriteChunk(f, "IHDR", ihdr, sizeof(ihdr))) {
    goto done;
  }
  uint8_t srgb = 0;  // perceptual
  if (!AGWriteChunk(f, "sRGB", &srgb, 1)) {
    goto done;
  }
  // zlib stream = 2-byte header + concatenated raw deflate bands + Adler-32 trailer.
  for (size_t i = 0; i < bandCount; i++) {
    size_t extra = (i == 0 ? 2 : 0) + (i == bandCount - 1 ? 4 : 0);
    size_t length = bands[i].outLength + extra;
    uint8_t *chunk = malloc(length);
    uint8_t *p = chunk;
    if (i == 0) {
      *p++ = 0x78;  // CMF: deflate, 32K window
      *p++ = 0x01;  // FLG: fastest, check bits
    }
    memcpy(p, bands[i].out, bands[i].outLength);
    p += bands[i].outLength;
    if (i == bandCount - 1) {
      AGPut32(p, (uint32_t)adler);
    }
    int ok = AGWriteChunk(f, "IDAT", chunk, length);
    free(chunk);
    if (!ok) {
      goto done;
    }
  }
  if (!AGWriteChunk(f, "IEND", NULL, 0)) {
    goto done;
  }
  written = ftell(f);

done:
  if (f) {
    if (fclose(f) != 0) {
      written = -1;
    }
  }
  for (size_t i = 0; i < bandCount; i++) {
    free(bands[i].out);
  }
  free(bands);
  return written;
}
