// Screenshots straight from the simulator's framebuffer IOSurface.
// PNG: agemu's parallel encoder (AGPNG.c). JPEG: ImageIO (hardware-accelerated).
import AgemuSim
import CoreGraphics
import CoreImage
import Foundation
import ImageIO
import IOSurface
import UniformTypeIdentifiers

/// A 32-bit little-endian BGRX pixel buffer (memory order B, G, R, X).
struct PixelBuffer {
    var data: Data
    let width: Int
    let height: Int
    let bytesPerRow: Int

    static let bitmapInfo = CGImageAlphaInfo.noneSkipFirst.rawValue | CGBitmapInfo.byteOrder32Little.rawValue

    func cgImage() throws -> CGImage {
        guard let provider = CGDataProvider(data: data as CFData),
              let image = CGImage(
                  width: width, height: height, bitsPerComponent: 8, bitsPerPixel: 32, bytesPerRow: bytesPerRow,
                  space: Screenshot.sRGB, bitmapInfo: CGBitmapInfo(rawValue: Self.bitmapInfo),
                  provider: provider, decode: nil, shouldInterpolate: false, intent: .defaultIntent)
        else { throw HelperError("could not wrap the framebuffer") }
        return image
    }

    func scaled(_ scale: Double) throws -> PixelBuffer {
        let w = max(1, Int((Double(width) * scale).rounded()))
        let h = max(1, Int((Double(height) * scale).rounded()))
        guard let ctx = CGContext(
            data: nil, width: w, height: h, bitsPerComponent: 8, bytesPerRow: w * 4, space: Screenshot.sRGB,
            bitmapInfo: Self.bitmapInfo)
        else { throw HelperError("could not create a scaling context") }
        ctx.interpolationQuality = .medium
        ctx.draw(try cgImage(), in: CGRect(x: 0, y: 0, width: w, height: h))
        guard let base = ctx.data else { throw HelperError("scaling failed") }
        return PixelBuffer(data: Data(bytes: base, count: w * 4 * h), width: w, height: h, bytesPerRow: w * 4)
    }
}

enum Screenshot {
    static let sRGB = CGColorSpace(name: CGColorSpace.sRGB)!
    static let ciContext = CIContext(options: [.cacheIntermediates: false])

    /// Copies the current framebuffer. The surface is locked only for the copy (~2 ms).
    static func pixels(_ surface: IOSurface) throws -> PixelBuffer {
        let width = surface.width, height = surface.height
        guard width > 0, height > 0 else { throw HelperError("empty framebuffer") }
        let format = surface.pixelFormat
        if format == 0x4247_5241 /* 'BGRA' */ {
            surface.lock(options: .readOnly, seed: nil)
            let bytesPerRow = surface.bytesPerRow
            let data = Data(bytes: surface.baseAddress, count: bytesPerRow * height)
            surface.unlock(options: .readOnly, seed: nil)
            return PixelBuffer(data: data, width: width, height: height, bytesPerRow: bytesPerRow)
        }
        // Any other pixel format (e.g. 10-bit or half-float wide colour): let Core Image convert it.
        let ci = CIImage(ioSurface: unsafeBitCast(surface, to: IOSurfaceRef.self))
        var data = Data(count: width * 4 * height)
        data.withUnsafeMutableBytes { buf in
            ciContext.render(ci, toBitmap: buf.baseAddress!, rowBytes: width * 4, bounds: ci.extent, format: .BGRA8, colorSpace: sRGB)
        }
        return PixelBuffer(data: data, width: width, height: height, bytesPerRow: width * 4)
    }

    static func capture(_ device: AGDevice, path: String, format: String, scale: Double, quality: Double) throws -> [String: Any] {
        let surface = try device.framebuffer()
        var px = try pixels(surface)
        if scale > 0, scale < 1 {
            px = try px.scaled(scale)
        }
        let jpeg = format == "jpeg" || format == "jpg"
        var size = 0
        if jpeg {
            let url = URL(fileURLWithPath: path)
            guard let dest = CGImageDestinationCreateWithURL(url as CFURL, UTType.jpeg.identifier as CFString, 1, nil) else {
                throw HelperError("cannot write \(path)")
            }
            CGImageDestinationAddImage(dest, try px.cgImage(), [kCGImageDestinationLossyCompressionQuality: quality] as CFDictionary)
            guard CGImageDestinationFinalize(dest) else { throw HelperError("encoding \(path) failed") }
            size = (try? FileManager.default.attributesOfItem(atPath: path)[.size] as? Int) ?? 0
        } else {
            let written = px.data.withUnsafeBytes { buf in
                AGWritePNG(buf.bindMemory(to: UInt8.self).baseAddress, px.width, px.height, px.bytesPerRow, path, 1)
            }
            guard written > 0 else { throw HelperError("writing PNG \(path) failed") }
            size = written
        }
        return ["path": path, "width": px.width, "height": px.height, "format": jpeg ? "jpeg" : "png", "bytes": size]
    }
}
