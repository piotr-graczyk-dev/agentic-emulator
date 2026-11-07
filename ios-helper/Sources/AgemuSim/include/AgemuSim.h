// AgemuSim: Objective-C glue over the CoreSimulator, SimulatorKit and
// AccessibilityPlatformTranslation private frameworks. Nothing is linked at build
// time: the frameworks are dlopen'ed and every class is looked up by name.
//
// Every entry point catches NSExceptions and turns them into NSErrors.
#import <Foundation/Foundation.h>
#import <CoreGraphics/CoreGraphics.h>
#import <IOSurface/IOSurfaceObjC.h>
#import "AGPNG.h"

NS_ASSUME_NONNULL_BEGIN

FOUNDATION_EXPORT NSErrorDomain const AGErrorDomain;

/// Loads the private frameworks once. Returns NO with an error if one is missing.
BOOL AGLoadFrameworks(NSError **error);

/// Version strings (CFBundleVersion) of the loaded CoreSimulator and SimulatorKit.
NSDictionary<NSString *, NSString *> *AGFrameworkVersions(void);

/// Indigo edge flags for touches (system edge gestures).
typedef NS_ENUM(uint32_t, AGEdge) {
  AGEdgeNone = 0,
  AGEdgeTop = 1,
  AGEdgeLeft = 2,
  AGEdgeBottom = 3,
  AGEdgeRight = 4,
};

/// One simulator. Instances are cached per UDID (see AGSimulators), so the
/// framebuffer surface and HID client stay connected between requests.
@interface AGDevice : NSObject

@property (nonatomic, readonly) NSString *udid;
@property (nonatomic, readonly) NSString *name;
/// CoreSimulator state: 0 creating, 1 shutdown, 2 booting, 3 booted, 4 shutting down.
@property (nonatomic, readonly) NSUInteger state;
@property (nonatomic, readonly) NSString *stateString;
@property (nonatomic, readonly) NSString *deviceTypeIdentifier;
@property (nonatomic, readonly) NSString *deviceTypeName;
@property (nonatomic, readonly) NSString *runtimeIdentifier;
@property (nonatomic, readonly) NSString *runtimeVersion;
@property (nonatomic, readonly) NSString *runtimeRoot;
@property (nonatomic, readonly) NSString *dataPath;
/// Main screen size in pixels, and the points-to-pixels scale.
@property (nonatomic, readonly) CGSize screenPixels;
@property (nonatomic, readonly) double screenScale;

/// CoreSimulator's boot progress (what `simctl bootstatus` reports): status (4 = waiting on
/// SpringBoard), terminal (YES once finished), elapsed (s). nil when unknown.
@property (nonatomic, readonly, nullable) NSDictionary *bootStatusInfo;
/// When the current boot started (-[SimDevice lastBootedAt]); changes with every boot.
@property (nonatomic, readonly, nullable) NSDate *lastBootedAt;

// Lifecycle
- (BOOL)bootWithError:(NSError **)error;
- (BOOL)shutdownWithError:(NSError **)error;

// Apps
- (pid_t)launch:(NSString *)bundleID
      arguments:(NSArray<NSString *> *)arguments
    environment:(NSDictionary<NSString *, NSString *> *)environment
          error:(NSError **)error;
- (BOOL)terminate:(NSString *)bundleID error:(NSError **)error;
- (BOOL)install:(NSString *)path error:(NSError **)error;
- (BOOL)uninstall:(NSString *)bundleID error:(NSError **)error;
- (nullable NSDictionary<NSString *, NSDictionary *> *)installedAppsWithError:(NSError **)error;
/// Runs a binary inside the simulator (path is relative to the runtime root) and returns its stdout.
- (nullable NSString *)spawnCapture:(NSString *)path
                          arguments:(NSArray<NSString *> *)arguments
                            timeout:(NSTimeInterval)timeout
                              error:(NSError **)error;
- (BOOL)postDarwinNotification:(NSString *)name error:(NSError **)error;

// Framebuffer
/// The live main-display framebuffer surface. The first call registers surface-change callbacks
/// and keeps them (and the surface) alive for the life of the process.
- (nullable IOSurface *)framebufferWithError:(NSError **)error;

// HID (SimDeviceLegacyHIDClient + Indigo messages)
/// A touch at a position given as a ratio of the screen (0..1, top-left origin).
- (BOOL)touchRatioX:(double)x y:(double)y down:(BOOL)down edge:(AGEdge)edge error:(NSError **)error;
/// A keyboard key by HID usage (page 7), e.g. 4 = 'a', 40 = return.
- (BOOL)key:(uint32_t)usage down:(BOOL)down error:(NSError **)error;
/// A hardware button: 0 = home, 1 = lock.
- (BOOL)button:(uint32_t)source down:(BOOL)down error:(NSError **)error;
/// A consumer-page HID usage (e.g. 0xE9 volume up).
- (BOOL)consumerUsage:(uint32_t)usage down:(BOOL)down error:(NSError **)error;
/// Connects the HID client now (the first connection takes about a second).
- (BOOL)prepareHIDWithError:(NSError **)error;
/// Waits until the display shows no new frame for quietMs (max maxMs): {settled, waitedMs, frames}.
- (NSDictionary *)waitForSettleQuiet:(double)quietMs max:(double)maxMs;
/// Like waitForSettleQuiet, but only big changes count (more than 0.4% of a sparse pixel grid):
/// a blinking cursor, a spinner or a countdown do not keep the screen from being still.
- (NSDictionary *)waitForStillQuiet:(double)quietMs max:(double)maxMs;
/// Waits for a frame newer than mark, then until there has been no big change for quietMs; at
/// most maxMs in all. Returns @{@"mark": frame counter}.
- (NSDictionary *)waitForFrameSince:(NSUInteger)mark quiet:(double)quietMs max:(double)maxMs;
/// Waits up to startMs for a big change to begin (from now), then until there has been no big
/// change for quietMs; at most maxMs in all. {changed, waitedMs}. For changes the system
/// animates after a delay (the appearance crossfade starts 50-100 ms after the call).
- (NSDictionary *)waitForBigChangeWithin:(double)startMs quiet:(double)quietMs max:(double)maxMs;
/// Waits until a tap on an element that covers rect (screen pixels) can land, for an element the
/// caller saw appear since the last input (a just-presented alert, sheet or screen). UIKit drops
/// every touch from the moment a presentation begins until its completion: the scene ignores
/// interaction events for the transition, and the touch never reaches the app.
///  1. If neither rect nor the screen (a big change) has changed since the last input, the
///     presentation's first frame may not have shown yet (it showed 32-146 ms after it began, at
///     load average 14-67): wait up to startMaxMs for either to change.
///  2. Then wait until no sample of a pixel grid in rect's column (its x range, full height) has
///     changed for quietMs and the screen has had no big change for stillQuietMs (0: not
///     required). An alert's or a sheet's completion comes 20-33 ms before its last change
///     shows; an action sheet's glass keeps changing after it. Once the screen is still, the
///     column may keep changing for at most tailMaxMs more (a spinner or a video never stops).
/// At most maxMs in all. {settled, pending (step 1 waited), waitedMs}.
- (NSDictionary *)waitForRectReady:(CGRect)rect
                             quiet:(double)quietMs
                        stillQuiet:(double)stillQuietMs
                          startMax:(double)startMaxMs
                           tailMax:(double)tailMaxMs
                               max:(double)maxMs;
/// When the last input was sent (a HID message, or markInput), CFAbsoluteTime; 0 before any.
@property (atomic, readonly) CFAbsoluteTime lastInputAt;
/// Marks input that is not a HID message (a launch, an opened URL).
- (void)markInput;
/// Frame counter to take before sending input (NSUIntegerMax without a framebuffer).
- (NSUInteger)inputFrameMark;
/// After input: waits for the first frame newer than `mark`, then for `quietMs` without frames;
/// at most `maxMs` in all.
- (NSDictionary *)waitForChangeSince:(NSUInteger)mark quiet:(double)quietMs max:(double)maxMs;
/// YES when the framebuffer surface / HID client of the current boot are connected.
@property (nonatomic, readonly) BOOL framebufferConnected;
@property (nonatomic, readonly) BOOL hidConnected;
/// Waits until every HID message sent so far has been acknowledged.
- (BOOL)hidWaitForAcknowledgements:(NSTimeInterval)timeout error:(NSError **)error;

// Accessibility (AXPTranslator). Every read is bounded by timeout (seconds, <= 0: 20 s) and
// fails with an error, never with a partial result, when the simulator does not answer in time.
/// The frontmost application's accessibility tree as nested dictionaries. Frames are in points.
/// maxDepth < 0 means unlimited.
- (nullable NSDictionary *)accessibilityTreeWithMaxDepth:(NSInteger)maxDepth
                                                 timeout:(double)timeout
                                                   error:(NSError **)error;
/// Hit-tests a point (in points) and returns that element's subtree plus its attribute names.
- (nullable NSDictionary *)accessibilityAtPointX:(double)x
                                               y:(double)y
                                        maxDepth:(NSInteger)maxDepth
                                         timeout:(double)timeout
                                           error:(NSError **)error;
/// A cheap readiness probe: the frontmost application's pid (one request), plus its frame and
/// label when full is YES.
- (nullable NSDictionary *)accessibilityFrontmostWithTimeout:(double)timeout full:(BOOL)full error:(NSError **)error;

@end

/// Test capabilities (AGCapabilities.m): CoreSimulator's own calls behind `simctl openurl,
/// privacy, location, ui appearance, push, pbcopy/pbpaste`, made in process.
@interface AGDevice (AGCapabilities)
- (BOOL)openURL:(NSString *)url error:(NSError **)error;
/// service is CoreSimulator's privacy service name: kTCCService* or __CoreLocationWhenInUse /
/// __CoreLocationAlways (simctl maps its names to these).
- (BOOL)setPrivacy:(NSString *)service bundleID:(NSString *)bundleID granted:(BOOL)granted error:(NSError **)error;
/// bundleID nil resets the service for every app.
- (BOOL)resetPrivacy:(NSString *)service bundleID:(nullable NSString *)bundleID error:(NSError **)error;
- (BOOL)setLocationLatitude:(double)latitude longitude:(double)longitude error:(NSError **)error;
- (BOOL)clearLocationWithError:(NSError **)error;
/// UIUserInterfaceStyle: 1 light, 2 dark; 0 unknown.
@property (nonatomic, readonly) NSInteger interfaceStyle;
- (BOOL)setInterfaceStyle:(NSInteger)style error:(NSError **)error;
- (BOOL)sendPush:(NSDictionary *)payload bundleID:(NSString *)bundleID error:(NSError **)error;
/// -[SimDevice propertiesOfApplication:]: ApplicationType, Path, DataContainer, GroupContainers...
- (nullable NSDictionary *)applicationProperties:(NSString *)bundleID error:(NSError **)error;
/// The device pasteboard's text ("" when it holds none).
- (nullable NSString *)pasteboardStringWithError:(NSError **)error;
/// Replaces the device pasteboard with text ("" empties it); returns once the device has it.
- (BOOL)setPasteboardString:(NSString *)text error:(NSError **)error;
/// Every item of the device pasteboard as {type: data}, and back.
- (nullable NSArray<NSDictionary<NSString *, NSData *> *> *)pasteboardItemsWithError:(NSError **)error;
- (BOOL)setPasteboardItems:(NSArray<NSDictionary<NSString *, NSData *> *> *)items error:(NSError **)error;
@end

@interface AGSimulators : NSObject
+ (nullable NSArray<AGDevice *> *)devicesWithError:(NSError **)error;
+ (nullable AGDevice *)deviceWithUDID:(NSString *)udid error:(NSError **)error;
@end

NS_ASSUME_NONNULL_END
