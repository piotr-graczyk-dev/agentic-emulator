// AgemuSim: CoreSimulator / SimulatorKit / AccessibilityPlatformTranslation glue.
//
// The private-API usage (Indigo HID message layout, framebuffer callbacks, the AXPTranslator
// token delegate) follows the public reverse-engineering in facebook/idb's FBSimulatorControl
// (MIT license, Copyright (c) Meta Platforms, Inc. and affiliates). The code here is agemu's own;
// where a byte layout is taken from idb's Indigo.h / SimulatorIndigoHID.swift it says so.
#import "AgemuSim.h"
#import "AGPrivate.h"

#import <dlfcn.h>
#import <mach/mach_time.h>
#import <poll.h>
#import <stdatomic.h>

NSErrorDomain const AGErrorDomain = @"agemu";

static NSError *AGError(NSString *format, ...) NS_FORMAT_FUNCTION(1, 2);
static NSError *AGError(NSString *format, ...)
{
  va_list ap;
  va_start(ap, format);
  NSString *message = [[NSString alloc] initWithFormat:format arguments:ap];
  va_end(ap);
  return [NSError errorWithDomain:AGErrorDomain code:1 userInfo:@{NSLocalizedDescriptionKey : message}];
}

static NSError *AGExceptionError(NSString *what, NSException *exception)
{
  return AGError(@"%@ raised %@: %@", what, exception.name, exception.reason);
}

static NSError *AGWrap(NSString *what, NSError *_Nullable underlying)
{
  if (!underlying) {
    return AGError(@"%@ failed", what);
  }
  return AGError(@"%@: %@", what, underlying.localizedDescription);
}

#define AG_FAIL(errp, err)                                                                                             \
  do {                                                                                                                 \
    if (errp) {                                                                                                        \
      *(errp) = (err);                                                                                                 \
    }                                                                                                                  \
  } while (0)

#pragma mark - Frameworks

// IndigoHIDMessageFor* builders exported by SimulatorKit. Signatures as documented in idb's
// SimulatorIndigoHID.swift (Xcode 26).
typedef void *(*AGIndigoMouseFn)(CGPoint *point1, CGPoint *point2, uint32_t target, unsigned long eventType,
                                 CGSize size, uint32_t edge);
typedef void *(*AGIndigoKeyboardFn)(int32_t keyCode, int32_t direction);
typedef void *(*AGIndigoButtonFn)(int32_t source, int32_t direction, int32_t target);
typedef void *(*AGIndigoHIDArbitraryFn)(int32_t target, uint32_t usagePage, uint32_t usage, int32_t direction);

static AGIndigoMouseFn gIndigoMouse;
static AGIndigoKeyboardFn gIndigoKeyboard;
static AGIndigoButtonFn gIndigoButton;
static AGIndigoHIDArbitraryFn gIndigoHIDArbitrary;
static NSString *gDeveloperDir;

static NSString *AGDeveloperDir(void)
{
  NSString *env = NSProcessInfo.processInfo.environment[@"DEVELOPER_DIR"];
  if (env.length > 0) {
    return env;
  }
  NSTask *task = [[NSTask alloc] init];
  task.executableURL = [NSURL fileURLWithPath:@"/usr/bin/xcode-select"];
  task.arguments = @[ @"-p" ];
  NSPipe *pipe = [NSPipe pipe];
  task.standardOutput = pipe;
  task.standardError = [NSFileHandle fileHandleWithNullDevice];
  if ([task launchAndReturnError:nil]) {
    NSData *data = [pipe.fileHandleForReading readDataToEndOfFile];
    [task waitUntilExit];
    NSString *path = [[[NSString alloc] initWithData:data encoding:NSUTF8StringEncoding]
      stringByTrimmingCharactersInSet:NSCharacterSet.whitespaceAndNewlineCharacterSet];
    if (task.terminationStatus == 0 && path.length > 0) {
      return path;
    }
  }
  return @"/Applications/Xcode.app/Contents/Developer";
}

static void *AGOpen(NSString *path, NSError **error)
{
  void *handle = dlopen(path.fileSystemRepresentation, RTLD_NOW | RTLD_GLOBAL);
  if (!handle) {
    const char *reason = dlerror();
    AG_FAIL(error, AGError(@"dlopen %@: %s", path, reason ? reason : "unknown error"));
  }
  return handle;
}

BOOL AGLoadFrameworks(NSError **error)
{
  static dispatch_once_t once;
  static NSError *loadError;
  dispatch_once(&once, ^{
    NSError *err = nil;
    gDeveloperDir = AGDeveloperDir();
    // AppKit first: AXPMacPlatformElement is an NSAccessibilityElement.
    [NSApplication class];
    if (!AGOpen(@"/Library/Developer/PrivateFrameworks/CoreSimulator.framework/CoreSimulator", &err)) {
      loadError = err;
      return;
    }
    NSString *simKit =
      [gDeveloperDir stringByAppendingPathComponent:@"Library/PrivateFrameworks/SimulatorKit.framework/SimulatorKit"];
    void *simKitHandle = AGOpen(simKit, &err);
    if (!simKitHandle) {
      loadError = err;
      return;
    }
    if (!AGOpen(@"/System/Library/PrivateFrameworks/AccessibilityPlatformTranslation.framework/"
                @"AccessibilityPlatformTranslation",
                &err)) {
      loadError = err;
      return;
    }
    gIndigoMouse = (AGIndigoMouseFn)dlsym(simKitHandle, "IndigoHIDMessageForMouseNSEvent");
    gIndigoKeyboard = (AGIndigoKeyboardFn)dlsym(simKitHandle, "IndigoHIDMessageForKeyboardArbitrary");
    gIndigoButton = (AGIndigoButtonFn)dlsym(simKitHandle, "IndigoHIDMessageForButton");
    gIndigoHIDArbitrary = (AGIndigoHIDArbitraryFn)dlsym(simKitHandle, "IndigoHIDMessageForHIDArbitrary");
    if (!gIndigoMouse || !gIndigoKeyboard || !gIndigoButton) {
      loadError = AGError(@"SimulatorKit at %@ lacks the IndigoHIDMessageFor* functions", simKit);
      return;
    }
  });
  if (loadError) {
    AG_FAIL(error, loadError);
    return NO;
  }
  return YES;
}

NSDictionary<NSString *, NSString *> *AGFrameworkVersions(void)
{
  NSMutableDictionary *out = [NSMutableDictionary dictionary];
  Class simDevice = NSClassFromString(@"SimDevice");
  if (simDevice) {
    out[@"coreSimulator"] = [NSBundle bundleForClass:simDevice].infoDictionary[@"CFBundleVersion"] ?: @"?";
  }
  NSBundle *simKit = [NSBundle bundleWithPath:[gDeveloperDir
                                                stringByAppendingPathComponent:
                                                  @"Library/PrivateFrameworks/SimulatorKit.framework"]];
  out[@"simulatorKit"] = simKit.infoDictionary[@"CFBundleVersion"] ?: @"?";
  out[@"developerDir"] = gDeveloperDir ?: @"";
  return out;
}

static NSString *AGStringValue(id value)
{
  if (!value || value == NSNull.null) {
    return nil;
  }
  if ([value isKindOfClass:NSString.class]) {
    return value;
  }
  if ([value isKindOfClass:NSAttributedString.class]) {
    return [(NSAttributedString *)value string];
  }
  if ([value isKindOfClass:NSNumber.class]) {
    return [(NSNumber *)value stringValue];
  }
  return [value description];
}

#pragma mark - Accessibility bridge

// AXPTranslator is a process-wide singleton with one token-delegate slot. Each tree read
// registers a token -> SimDevice mapping; the translator calls back with the token for every
// lazily fetched attribute, and we forward the request to that device via CoreSimulator.
@interface AGAXBridge : NSObject <AGAXPTranslationTokenDelegateHelper>
@property (nonatomic, strong, readonly) dispatch_queue_t callbackQueue;
@property (nonatomic, strong, readonly) dispatch_queue_t workQueue;
@property (nonatomic, strong, readonly) NSMutableDictionary<NSString *, id> *devices;
@property (nonatomic, strong, readonly) id translator;
@property (atomic, assign) NSUInteger requestCount;
@end

@implementation AGAXBridge

+ (instancetype)shared
{
  static AGAXBridge *bridge;
  static dispatch_once_t once;
  dispatch_once(&once, ^{
    bridge = [[AGAXBridge alloc] init];
  });
  return bridge;
}

- (instancetype)init
{
  self = [super init];
  if (!self) {
    return nil;
  }
  _callbackQueue = dispatch_queue_create("agemu.ax.callback", DISPATCH_QUEUE_CONCURRENT);
  _workQueue = dispatch_queue_create("agemu.ax.work", DISPATCH_QUEUE_SERIAL);
  _devices = [NSMutableDictionary dictionary];
  Class translatorClass = NSClassFromString(@"AXPTranslator");
  _translator = [(id<AGAXPTranslator>)translatorClass sharedInstance];
  [(id<AGAXPTranslator>)_translator setBridgeTokenDelegate:self];
  return self;
}

- (NSString *)pushDevice:(id)device
{
  NSString *token = NSUUID.UUID.UUIDString;
  @synchronized(self.devices) {
    self.devices[token] = device;
  }
  return token;
}

- (void)popToken:(NSString *)token
{
  @synchronized(self.devices) {
    [self.devices removeObjectForKey:token];
  }
}

- (AGTranslationCallback)accessibilityTranslationDelegateBridgeCallbackWithToken:(NSString *)token
{
  id device;
  @synchronized(self.devices) {
    device = self.devices[token];
  }
  Class responseClass = NSClassFromString(@"AXPTranslatorResponse");
  if (!device) {
    return ^id(id request) {
      return [(id<AGAXPTranslatorResponse>)responseClass emptyResponse];
    };
  }
  dispatch_queue_t queue = self.callbackQueue;
  static BOOL debug;
  static dispatch_once_t once;
  dispatch_once(&once, ^{
    debug = getenv("AGEMU_AX_DEBUG") != NULL;
  });
  return ^id(id request) {
    self.requestCount += 1;
    CFAbsoluteTime start = CFAbsoluteTimeGetCurrent();
    if (debug) {
      fprintf(stderr, "ax> %s\n", [[request description] UTF8String]);
    }
    dispatch_semaphore_t done = dispatch_semaphore_create(0);
    __block id response = nil;
    [(id<AGSimDevice>)device sendAccessibilityRequestAsync:request
                                           completionQueue:queue
                                         completionHandler:^(id inner) {
                                           response = inner;
                                           dispatch_semaphore_signal(done);
                                         }];
    if (dispatch_semaphore_wait(done, dispatch_time(DISPATCH_TIME_NOW, (int64_t)(5 * NSEC_PER_SEC))) != 0 || !response) {
      return [(id<AGAXPTranslatorResponse>)responseClass emptyResponse];
    }
    if (debug) {
      fprintf(stderr, "ax< %.1f ms %s\n", (CFAbsoluteTimeGetCurrent() - start) * 1000, [[[response description] substringToIndex:MIN((NSUInteger)200, [[response description] length])] UTF8String]);
    }
    return response;
  };
}

- (CGRect)accessibilityTranslationConvertPlatformFrameToSystem:(CGRect)rect withToken:(NSString *)token
{
  // Keep iOS screen points (top-left origin); the Go side converts to pixels.
  return rect;
}

- (id)accessibilityTranslationRootParentWithToken:(NSString *)token
{
  return nil;
}

@end

static NSDictionary *AGAXNode(id element, NSString *token, NSInteger depth, NSInteger maxDepth, NSUInteger *count);

// Some containers report no children although they have some (e.g. the iOS 26 Settings
// toolbar that hosts the search field: both AXChildren and AXChildrenInNavigationOrder come
// back empty). Hit-testing inside the container still finds them, so sample three points
// along its horizontal centre line and keep distinct elements that lie inside its frame.
static NSArray *AGAXHiddenChildren(NSRect frame, NSString *token, NSInteger depth, NSInteger maxDepth, NSUInteger *count)
{
  if (frame.size.width < 1 || frame.size.height < 1) {
    return @[];
  }
  AGAXBridge *bridge = AGAXBridge.shared;
  id<AGAXPTranslator> translator = bridge.translator;
  NSMutableArray *out = [NSMutableArray array];
  NSMutableSet *seen = [NSMutableSet set];
  NSRect bounds = NSInsetRect(frame, -1, -1);
  for (NSNumber *fx in @[ @0.5, @0.17, @0.83 ]) {
    CGPoint point = CGPointMake(NSMinX(frame) + fx.doubleValue * NSWidth(frame), NSMidY(frame));
    id translation = [translator objectAtPoint:point displayId:0 bridgeDelegateToken:token];
    if (!translation) {
      continue;
    }
    [(id<AGAXPTranslationObject>)translation setBridgeDelegateToken:token];
    NSNumber *objectID = @([(id<AGAXPTranslationObject>)translation objectID]);
    if ([seen containsObject:objectID]) {
      continue;
    }
    [seen addObject:objectID];
    id element = [translator macPlatformElementFromTranslation:translation];
    if (!element) {
      continue;
    }
    [(id<AGAXPTranslationObject>)[(id<AGAXPMacPlatformElement>)element translation] setBridgeDelegateToken:token];
    NSRect hit = [(NSAccessibilityElement *)element accessibilityFrame];
    if (NSIsEmptyRect(hit) || !NSContainsRect(bounds, hit) || NSEqualRects(hit, frame)) {
      continue;
    }
    NSDictionary *node = AGAXNode(element, token, depth, maxDepth, count);
    if (node) {
      [out addObject:node];
    }
  }
  return out;
}

// Reads one element. Each NSAccessibility getter is one translator round trip, so only the
// attributes agemu maps to api.Element are read.
static NSDictionary *AGAXNode(id element, NSString *token, NSInteger depth, NSInteger maxDepth, NSUInteger *count)
{
  [(id<AGAXPTranslationObject>)[(id<AGAXPMacPlatformElement>)element translation] setBridgeDelegateToken:token];
  NSAccessibilityElement *e = element;
  NSMutableDictionary *node = [NSMutableDictionary dictionary];
  *count += 1;
  NSRect frame = e.accessibilityFrame;
  node[@"frame"] = @[ @(frame.origin.x), @(frame.origin.y), @(frame.size.width), @(frame.size.height) ];
  NSString *role = e.accessibilityRole;
  if (role) {
    node[@"role"] = role;
  }
  NSString *subrole = e.accessibilitySubrole;
  if (subrole) {
    node[@"subrole"] = subrole;
  }
  NSString *label = AGStringValue(e.accessibilityLabel);
  if (label.length) {
    node[@"label"] = label;
  }
  NSString *identifier = AGStringValue(e.accessibilityIdentifier);
  if (identifier.length) {
    node[@"id"] = identifier;
  }
  NSString *value = AGStringValue(e.accessibilityValue);
  if (value.length) {
    node[@"value"] = value;
  }
  NSString *title = AGStringValue(e.accessibilityTitle);
  if (title.length) {
    node[@"title"] = title;
  }
  node[@"enabled"] = @(e.isAccessibilityEnabled);
  if (e.isAccessibilitySelected) {
    node[@"selected"] = @YES;
  }
  if (e.isAccessibilityFocused) {
    node[@"focused"] = @YES;
  }
  if (maxDepth < 0 || depth < maxDepth) {
    NSArray *children = e.accessibilityChildren;
    NSMutableArray *out = [NSMutableArray arrayWithCapacity:children.count];
    for (id child in children) {
      NSDictionary *c = AGAXNode(child, token, depth + 1, maxDepth, count);
      if (c) {
        [out addObject:c];
      }
    }
    if (out.count == 0 && depth > 0 && [role isEqualToString:@"AXGroup"]) {
      [out addObjectsFromArray:AGAXHiddenChildren(frame, token, depth + 1, maxDepth, count)];
      if (out.count) {
        node[@"hitTested"] = @YES;
      }
    }
    if (out.count) {
      node[@"children"] = out;
    }
  }
  return node;
}

#pragma mark - AGDevice

@interface AGDevice ()
@property (nonatomic, strong) id<AGSimDevice> device;
// framebuffer
@property (nonatomic, strong, readonly) dispatch_queue_t framebufferSetupQueue;
@property (nonatomic, strong, readonly) dispatch_queue_t framebufferCallbackQueue;
@property (nonatomic, strong, nullable) id display;
@property (nonatomic, strong, nullable) IOSurface *surface;
@property (nonatomic, strong, readonly) NSUUID *callbackUUID;
// HID
@property (nonatomic, strong, readonly) dispatch_queue_t hidQueue;
@property (nonatomic, strong, readonly) dispatch_queue_t hidAckQueue;
@property (nonatomic, strong, nullable) id hidClient;
@property (nonatomic, strong) dispatch_group_t hidGroup;
@property (nonatomic, strong, nullable) NSError *hidDeliveryError;
@end

@implementation AGDevice

- (instancetype)initWithDevice:(id)device
{
  self = [super init];
  if (!self) {
    return nil;
  }
  _device = device;
  _framebufferSetupQueue = dispatch_queue_create("agemu.framebuffer.setup", DISPATCH_QUEUE_SERIAL);
  _framebufferCallbackQueue =
    dispatch_queue_create("agemu.framebuffer.callbacks",
                          dispatch_queue_attr_make_with_qos_class(DISPATCH_QUEUE_SERIAL, QOS_CLASS_USER_INTERACTIVE, 0));
  _callbackUUID = NSUUID.UUID;
  _hidQueue = dispatch_queue_create("agemu.hid", DISPATCH_QUEUE_SERIAL);
  _hidAckQueue = dispatch_queue_create("agemu.hid.ack", DISPATCH_QUEUE_SERIAL);
  _hidGroup = dispatch_group_create();
  return self;
}

- (NSString *)udid
{
  return self.device.UDID.UUIDString;
}

- (NSString *)name
{
  return self.device.name;
}

- (NSUInteger)state
{
  @try {
    return (NSUInteger)self.device.state;
  } @catch (NSException *e) {
    return 0;
  }
}

- (NSString *)stateString
{
  @try {
    return self.device.stateString ?: @"unknown";
  } @catch (NSException *e) {
    return @"unknown";
  }
}

- (NSString *)deviceTypeIdentifier
{
  return [(id<AGSimDeviceType>)self.device.deviceType identifier] ?: @"";
}

- (NSString *)deviceTypeName
{
  return [(id<AGSimDeviceType>)self.device.deviceType name] ?: @"";
}

- (NSString *)runtimeIdentifier
{
  return [(id<AGSimRuntime>)self.device.runtime identifier] ?: @"";
}

- (NSString *)runtimeVersion
{
  return [(id<AGSimRuntime>)self.device.runtime versionString] ?: @"";
}

- (NSString *)runtimeRoot
{
  return [(id<AGSimRuntime>)self.device.runtime root] ?: @"";
}

- (NSString *)dataPath
{
  return self.device.dataPath ?: @"";
}

- (CGSize)screenPixels
{
  return [(id<AGSimDeviceType>)self.device.deviceType mainScreenSize];
}

- (double)screenScale
{
  return [(id<AGSimDeviceType>)self.device.deviceType mainScreenScale];
}

#pragma mark Lifecycle

- (BOOL)bootWithError:(NSError **)error
{
  @try {
    NSError *err = nil;
    // persist: keep the device booted after this process exits (as `simctl boot` does).
    if (![self.device bootWithOptions:@{@"persist" : @YES} error:&err]) {
      AG_FAIL(error, AGWrap(@"boot", err));
      return NO;
    }
    return YES;
  } @catch (NSException *e) {
    AG_FAIL(error, AGExceptionError(@"boot", e));
    return NO;
  }
}

- (BOOL)shutdownWithError:(NSError **)error
{
  [self resetConnections];
  @try {
    NSError *err = nil;
    if (![self.device shutdownWithError:&err]) {
      AG_FAIL(error, AGWrap(@"shutdown", err));
      return NO;
    }
    return YES;
  } @catch (NSException *e) {
    AG_FAIL(error, AGExceptionError(@"shutdown", e));
    return NO;
  }
}

// Drops the framebuffer and HID connections; they are re-created on next use (after a reboot
// the old proxies are dead).
- (void)resetConnections
{
  dispatch_sync(self.framebufferSetupQueue, ^{
    @synchronized(self) {
      self.surface = nil;
      self.display = nil;
    }
  });
  dispatch_sync(self.hidQueue, ^{
    self.hidClient = nil;
    self.hidGroup = dispatch_group_create();
  });
}

#pragma mark Apps

- (pid_t)launch:(NSString *)bundleID
      arguments:(NSArray<NSString *> *)arguments
    environment:(NSDictionary<NSString *, NSString *> *)environment
          error:(NSError **)error
{
  @try {
    NSError *err = nil;
    NSDictionary *options = @{@"arguments" : arguments ?: @[], @"environment" : environment ?: @{}};
    int pid = [self.device launchApplicationWithID:bundleID options:options error:&err];
    if (pid <= 0) {
      AG_FAIL(error, AGWrap([NSString stringWithFormat:@"launch %@", bundleID], err));
      return -1;
    }
    return pid;
  } @catch (NSException *e) {
    AG_FAIL(error, AGExceptionError(@"launch", e));
    return -1;
  }
}

- (BOOL)terminate:(NSString *)bundleID error:(NSError **)error
{
  @try {
    NSError *err = nil;
    if (![self.device terminateApplicationWithID:bundleID error:&err]) {
      AG_FAIL(error, AGWrap([NSString stringWithFormat:@"terminate %@", bundleID], err));
      return NO;
    }
    return YES;
  } @catch (NSException *e) {
    AG_FAIL(error, AGExceptionError(@"terminate", e));
    return NO;
  }
}

- (BOOL)install:(NSString *)path error:(NSError **)error
{
  @try {
    NSError *err = nil;
    if (![self.device installApplication:[NSURL fileURLWithPath:path] withOptions:@{} error:&err]) {
      AG_FAIL(error, AGWrap([NSString stringWithFormat:@"install %@", path], err));
      return NO;
    }
    return YES;
  } @catch (NSException *e) {
    AG_FAIL(error, AGExceptionError(@"install", e));
    return NO;
  }
}

- (BOOL)uninstall:(NSString *)bundleID error:(NSError **)error
{
  @try {
    NSError *err = nil;
    if (![self.device uninstallApplication:bundleID withOptions:@{} error:&err]) {
      AG_FAIL(error, AGWrap([NSString stringWithFormat:@"uninstall %@", bundleID], err));
      return NO;
    }
    return YES;
  } @catch (NSException *e) {
    AG_FAIL(error, AGExceptionError(@"uninstall", e));
    return NO;
  }
}

- (NSDictionary<NSString *, NSDictionary *> *)installedAppsWithError:(NSError **)error
{
  @try {
    NSError *err = nil;
    NSDictionary *apps = [self.device installedAppsWithError:&err];
    if (!apps) {
      AG_FAIL(error, AGWrap(@"installedApps", err));
      return nil;
    }
    return apps;
  } @catch (NSException *e) {
    AG_FAIL(error, AGExceptionError(@"installedApps", e));
    return nil;
  }
}

- (NSString *)spawnCapture:(NSString *)path
                 arguments:(NSArray<NSString *> *)arguments
                   timeout:(NSTimeInterval)timeout
                     error:(NSError **)error
{
  int fds[2];
  if (pipe(fds) != 0) {
    AG_FAIL(error, AGError(@"pipe: %s", strerror(errno)));
    return nil;
  }
  NSString *full = [self.runtimeRoot stringByAppendingPathComponent:path];
  NSMutableArray *argv = [NSMutableArray arrayWithObject:full];
  [argv addObjectsFromArray:arguments];
  NSDictionary *options = @{@"arguments" : argv, @"environment" : @{}, @"stdout" : @(fds[1]), @"stderr" : @(fds[1])};
  dispatch_semaphore_t exited = dispatch_semaphore_create(0);
  int pid = -1;
  NSError *err = nil;
  @try {
    pid = [self.device spawnWithPath:full
                             options:options
                    terminationQueue:dispatch_get_global_queue(QOS_CLASS_USER_INITIATED, 0)
                  terminationHandler:^(int status) {
                    dispatch_semaphore_signal(exited);
                  }
                               error:&err];
  } @catch (NSException *e) {
    close(fds[0]);
    close(fds[1]);
    AG_FAIL(error, AGExceptionError(@"spawn", e));
    return nil;
  }
  close(fds[1]);
  if (pid <= 0) {
    close(fds[0]);
    AG_FAIL(error, AGWrap([NSString stringWithFormat:@"spawn %@", path], err));
    return nil;
  }
  NSMutableData *out = [NSMutableData data];
  NSDate *deadline = [NSDate dateWithTimeIntervalSinceNow:timeout];
  char buf[16384];
  for (;;) {
    int remaining = (int)MAX(0, [deadline timeIntervalSinceNow] * 1000);
    if (remaining == 0) {
      close(fds[0]);
      AG_FAIL(error, AGError(@"spawn %@: timed out", path));
      return nil;
    }
    struct pollfd p = {.fd = fds[0], .events = POLLIN};
    int r = poll(&p, 1, remaining);
    if (r <= 0) {
      continue;
    }
    ssize_t n = read(fds[0], buf, sizeof(buf));
    if (n <= 0) {
      break;
    }
    [out appendBytes:buf length:(NSUInteger)n];
  }
  close(fds[0]);
  dispatch_semaphore_wait(exited, dispatch_time(DISPATCH_TIME_NOW, (int64_t)(timeout * NSEC_PER_SEC)));
  return [[NSString alloc] initWithData:out encoding:NSUTF8StringEncoding] ?: @"";
}

- (BOOL)postDarwinNotification:(NSString *)name error:(NSError **)error
{
  @try {
    NSError *err = nil;
    if (![self.device postDarwinNotification:name error:&err]) {
      AG_FAIL(error, AGWrap(@"postDarwinNotification", err));
      return NO;
    }
    return YES;
  } @catch (NSException *e) {
    AG_FAIL(error, AGExceptionError(@"postDarwinNotification", e));
    return NO;
  }
}

#pragma mark Framebuffer

static IOSurface *AGAsSurface(id candidate)
{
  if (!candidate) {
    return nil;
  }
  if (CFGetTypeID((__bridge CFTypeRef)candidate) == IOSurfaceGetTypeID()) {
    return (IOSurface *)candidate;
  }
  return nil;
}

- (void)setLiveSurface:(id)candidate
{
  IOSurface *surface = AGAsSurface(candidate);
  if (!surface) {
    return;
  }
  @synchronized(self) {
    self.surface = surface;
  }
}

- (IOSurface *)framebufferWithError:(NSError **)error
{
  @synchronized(self) {
    if (self.surface) {
      return self.surface;
    }
  }
  __block NSError *err = nil;
  dispatch_sync(self.framebufferSetupQueue, ^{
    @synchronized(self) {
      if (self.surface) {
        return;
      }
    }
    @try {
      if (self.device.state != 3) {
        err = AGError(@"device %@ is not booted (%@)", self.udid, self.stateString);
        return;
      }
      id<AGSimDeviceIO> io = self.device.io;
      if (!io) {
        err = AGError(@"device %@ has no IO client", self.udid);
        return;
      }
      id<AGSimDisplay> main = nil;
      id<AGSimDisplay> fallback = nil;
      for (id<AGSimDeviceIOPort> port in io.ioPorts) {
        id descriptor = port.descriptor;
        if (![descriptor respondsToSelector:@selector(framebufferSurface)] ||
            ![descriptor respondsToSelector:@selector(state)]) {
          continue;
        }
        id state = [(id<AGSimDisplay>)descriptor state];
        if ([state respondsToSelector:@selector(displayClass)] &&
            [(id<AGSimDisplayDescriptorState>)state displayClass] == 0) {
          main = descriptor;
          break;
        }
        if (!fallback) {
          fallback = descriptor;
        }
      }
      if (!main) {
        main = fallback;
      }
      if (!main) {
        err = AGError(@"no display port found on %@", self.udid);
        return;
      }
      self.display = main;
      __weak AGDevice *weakSelf = self;
      BOOL registered = NO;
      // New-style SimScreen callbacks, then the legacy plural/singular variants. Each is
      // guarded: the descriptor is a remote proxy that may not implement all of them.
      @try {
        [main registerScreenCallbacksWithUUID:self.callbackUUID
                                callbackQueue:self.framebufferCallbackQueue
                                frameCallback:^{
                                }
                      surfacesChangedCallback:^(id framebuffer, id masked) {
                        [weakSelf setLiveSurface:framebuffer];
                      }
                    propertiesChangedCallback:^(id properties){
                    }];
        registered = YES;
      } @catch (NSException *e) {
      }
      if (!registered) {
        @try {
          [main registerCallbackWithUUID:self.callbackUUID
                ioSurfacesChangeCallback:^(id surface) {
                  [weakSelf setLiveSurface:surface];
                }];
          registered = YES;
        } @catch (NSException *e) {
        }
        @try {
          [main registerCallbackWithUUID:self.callbackUUID
                 ioSurfaceChangeCallback:^(id surface) {
                   [weakSelf setLiveSurface:surface];
                 }];
          registered = YES;
        } @catch (NSException *e) {
        }
      }
      id current = nil;
      @try {
        current = [main framebufferSurface];
      } @catch (NSException *e) {
      }
      if (!AGAsSurface(current)) {
        @try {
          current = [main ioSurface];
        } @catch (NSException *e) {
        }
      }
      [self setLiveSurface:current];
    } @catch (NSException *e) {
      err = AGExceptionError(@"framebuffer", e);
    }
  });
  @synchronized(self) {
    if (self.surface) {
      return self.surface;
    }
  }
  AG_FAIL(error, err ?: AGError(@"no framebuffer surface for %@ yet", self.udid));
  return nil;
}

#pragma mark HID

- (BOOL)ensureHIDClient:(NSError **)error
{
  if (self.hidClient) {
    return YES;
  }
  Class cls = NSClassFromString(@"SimulatorKit.SimDeviceLegacyHIDClient")
                ?: NSClassFromString(@"_TtC12SimulatorKit24SimDeviceLegacyHIDClient")
                ?: NSClassFromString(@"SimDeviceLegacyHIDClient");
  if (!cls) {
    AG_FAIL(error, AGError(@"SimDeviceLegacyHIDClient class not found"));
    return NO;
  }
  @try {
    NSError *err = nil;
    id client = [(id<AGLegacyHIDClient>)[cls alloc] initWithDevice:self.device error:&err];
    if (!client) {
      AG_FAIL(error, AGWrap(@"SimDeviceLegacyHIDClient init", err));
      return NO;
    }
    self.hidClient = client;
    return YES;
  } @catch (NSException *e) {
    AG_FAIL(error, AGExceptionError(@"SimDeviceLegacyHIDClient init", e));
    return NO;
  }
}

// Hands a malloc'ed Indigo message to the HID client, which frees it.
- (BOOL)sendIndigoMessage:(void *)message error:(NSError **)error
{
  if (!message) {
    AG_FAIL(error, AGError(@"Indigo message builder returned NULL"));
    return NO;
  }
  __block BOOL ok = NO;
  __block NSError *err = nil;
  dispatch_sync(self.hidQueue, ^{
    if (![self ensureHIDClient:&err]) {
      free(message);
      return;
    }
    dispatch_group_t group = self.hidGroup;
    dispatch_group_enter(group);
    @try {
      [(id<AGLegacyHIDClient>)self.hidClient sendWithMessage:message
                                                freeWhenDone:YES
                                             completionQueue:self.hidAckQueue
                                                  completion:^(NSError *deliveryError) {
                                                    if (deliveryError && !self.hidDeliveryError) {
                                                      self.hidDeliveryError = deliveryError;
                                                    }
                                                    dispatch_group_leave(group);
                                                  }];
      ok = YES;
    } @catch (NSException *e) {
      dispatch_group_leave(group);
      // The client may be bound to a device that rebooted; drop it so the next send reconnects.
      self.hidClient = nil;
      err = AGExceptionError(@"HID send", e);
    }
  });
  if (!ok) {
    AG_FAIL(error, err);
  }
  return ok;
}

- (BOOL)hidWaitForAcknowledgements:(NSTimeInterval)timeout error:(NSError **)error
{
  __block dispatch_group_t group;
  dispatch_sync(self.hidQueue, ^{
    group = self.hidGroup;
  });
  if (dispatch_group_wait(group, dispatch_time(DISPATCH_TIME_NOW, (int64_t)(timeout * NSEC_PER_SEC))) != 0) {
    dispatch_sync(self.hidQueue, ^{
      // Abandon the stuck group so later waits are not poisoned.
      self.hidGroup = dispatch_group_create();
    });
    AG_FAIL(error, AGError(@"HID events were not acknowledged within %.1fs", timeout));
    return NO;
  }
  __block NSError *delivery = nil;
  dispatch_sync(self.hidAckQueue, ^{
    delivery = self.hidDeliveryError;
    self.hidDeliveryError = nil;
  });
  if (delivery) {
    AG_FAIL(error, AGWrap(@"HID delivery", delivery));
    return NO;
  }
  return YES;
}

// Builds a single-touch Indigo message. Layout from idb's Indigo.h / SimulatorIndigoHID.swift
// (MIT, Meta Platforms): SimulatorKit only builds multi-touch messages, so the digitizer payload
// is taken from IndigoHIDMessageForMouseNSEvent and re-enveloped as a two-payload single-touch
// message (eventType 2, eventKind 0xB), the second payload marked field1=1, field2=2.
static void *AGTouchMessage(double x, double y, BOOL down, uint32_t edge)
{
  enum {
    kMessageSize = 0xB0,  // sizeof(IndigoMessage), pack(4)
    kPayloadSize = 0x90,  // sizeof(IndigoPayload)
    kTouchSize = 0x70,    // sizeof(IndigoTouch)
    kPayloadOffset = 0x20,
    kEventOffset = 0x30,
  };
  CGPoint point = CGPointMake(x, y);
  uint8_t *source = gIndigoMouse(&point, NULL, 0x32, down ? 1 : 2, CGSizeMake(1, 1), edge);
  if (!source) {
    return NULL;
  }
  uint8_t *message = calloc(1, kMessageSize + kPayloadSize);
  uint32_t innerSize = kPayloadSize;
  memcpy(message + 0x18, &innerSize, 4);  // innerSize
  message[0x1c] = 2;                      // eventType: single touch
  uint32_t eventKind = 0xB;
  memcpy(message + kPayloadOffset, &eventKind, 4);
  uint64_t timestamp = mach_absolute_time();
  memcpy(message + kPayloadOffset + 4, &timestamp, 8);
  memcpy(message + kEventOffset, source + kEventOffset, kTouchSize);
  memcpy(message + kEventOffset + 0x0c, &x, 8);  // xRatio
  memcpy(message + kEventOffset + 0x14, &y, 8);  // yRatio
  free(source);
  uint8_t *second = message + kPayloadOffset + kPayloadSize;
  memcpy(second, message + kPayloadOffset, kPayloadSize);
  uint32_t one = 1, two = 2;
  memcpy(second + 0x10, &one, 4);
  memcpy(second + 0x14, &two, 4);
  return message;
}

- (BOOL)touchRatioX:(double)x y:(double)y down:(BOOL)down edge:(AGEdge)edge error:(NSError **)error
{
  if (!AGLoadFrameworks(error)) {
    return NO;
  }
  x = MIN(MAX(x, 0), 1);
  y = MIN(MAX(y, 0), 1);
  return [self sendIndigoMessage:AGTouchMessage(x, y, down, edge) error:error];
}

- (BOOL)key:(uint32_t)usage down:(BOOL)down error:(NSError **)error
{
  if (!AGLoadFrameworks(error)) {
    return NO;
  }
  return [self sendIndigoMessage:gIndigoKeyboard((int32_t)usage, down ? 1 : 2) error:error];
}

- (BOOL)button:(uint32_t)source down:(BOOL)down error:(NSError **)error
{
  if (!AGLoadFrameworks(error)) {
    return NO;
  }
  return [self sendIndigoMessage:gIndigoButton((int32_t)source, down ? 1 : 2, 0x33) error:error];
}

- (BOOL)consumerUsage:(uint32_t)usage down:(BOOL)down error:(NSError **)error
{
  if (!AGLoadFrameworks(error)) {
    return NO;
  }
  if (!gIndigoHIDArbitrary) {
    AG_FAIL(error, AGError(@"IndigoHIDMessageForHIDArbitrary is not available"));
    return NO;
  }
  return [self sendIndigoMessage:gIndigoHIDArbitrary(0x32, 0x0C, usage, down ? 1 : 2) error:error];
}

#pragma mark Accessibility

- (id)frontmostElementWithBridge:(AGAXBridge *)bridge token:(NSString *)token pid:(int *)pid error:(NSError **)error
{
  id<AGAXPTranslator> translator = bridge.translator;
  id translation = [translator frontmostApplicationWithDisplayId:0 bridgeDelegateToken:token];
  if (!translation) {
    AG_FAIL(error, AGError(@"no frontmost application (is SpringBoard running?)"));
    return nil;
  }
  [(id<AGAXPTranslationObject>)translation setBridgeDelegateToken:token];
  *pid = [(id<AGAXPTranslationObject>)translation pid];
  id element = [translator macPlatformElementFromTranslation:translation];
  if (!element) {
    AG_FAIL(error, AGError(@"no accessibility element for the frontmost application"));
    return nil;
  }
  [(id<AGAXPTranslationObject>)[(id<AGAXPMacPlatformElement>)element translation] setBridgeDelegateToken:token];
  return element;
}

- (NSDictionary *)accessibilityTreeWithMaxDepth:(NSInteger)maxDepth error:(NSError **)error
{
  if (!AGLoadFrameworks(error)) {
    return nil;
  }
  if (self.device.state != 3) {
    AG_FAIL(error, AGError(@"device %@ is not booted (%@)", self.udid, self.stateString));
    return nil;
  }
  AGAXBridge *bridge = AGAXBridge.shared;
  __block NSDictionary *result = nil;
  __block NSError *err = nil;
  dispatch_sync(bridge.workQueue, ^{
    NSString *token = [bridge pushDevice:self.device];
    NSUInteger before = bridge.requestCount;
    @try {
      int pid = 0;
      id element = [self frontmostElementWithBridge:bridge token:token pid:&pid error:&err];
      if (element) {
        NSUInteger count = 0;
        NSDictionary *root = AGAXNode(element, token, 0, maxDepth, &count);
        result = @{
          @"pid" : @(pid),
          @"root" : root,
          @"elements" : @(count),
          @"requests" : @(bridge.requestCount - before),
        };
      }
    } @catch (NSException *e) {
      err = AGExceptionError(@"accessibility", e);
    } @finally {
      [bridge popToken:token];
    }
  });
  if (!result) {
    AG_FAIL(error, err ?: AGError(@"accessibility read failed"));
  }
  return result;
}

- (NSDictionary *)accessibilityAtPointX:(double)x y:(double)y maxDepth:(NSInteger)maxDepth error:(NSError **)error
{
  if (!AGLoadFrameworks(error)) {
    return nil;
  }
  AGAXBridge *bridge = AGAXBridge.shared;
  __block NSDictionary *result = nil;
  __block NSError *err = nil;
  dispatch_sync(bridge.workQueue, ^{
    NSString *token = [bridge pushDevice:self.device];
    @try {
      id<AGAXPTranslator> translator = bridge.translator;
      id translation = [translator objectAtPoint:CGPointMake(x, y) displayId:0 bridgeDelegateToken:token];
      if (!translation) {
        err = AGError(@"nothing at %.0f,%.0f", x, y);
        return;
      }
      [(id<AGAXPTranslationObject>)translation setBridgeDelegateToken:token];
      id element = [translator macPlatformElementFromTranslation:translation];
      [(id<AGAXPTranslationObject>)[(id<AGAXPMacPlatformElement>)element translation] setBridgeDelegateToken:token];
      NSUInteger count = 0;
      NSMutableDictionary *node = [AGAXNode(element, token, 0, maxDepth, &count) mutableCopy];
      NSArray *names = [(NSAccessibilityElement *)element accessibilityAttributeNames];
      node[@"attributeNames"] = names ?: @[];
      id parent = [(NSAccessibilityElement *)element accessibilityParent];
      if (parent) {
        node[@"parentRole"] = AGStringValue([(NSAccessibilityElement *)parent accessibilityRole]) ?: @"";
      }
      result = node;
    } @catch (NSException *e) {
      err = AGExceptionError(@"accessibility", e);
    } @finally {
      [bridge popToken:token];
    }
  });
  if (!result) {
    AG_FAIL(error, err ?: AGError(@"accessibility read failed"));
  }
  return result;
}

- (NSDictionary *)accessibilityFrontmostWithError:(NSError **)error
{
  if (!AGLoadFrameworks(error)) {
    return nil;
  }
  if (self.device.state != 3) {
    AG_FAIL(error, AGError(@"device %@ is not booted (%@)", self.udid, self.stateString));
    return nil;
  }
  AGAXBridge *bridge = AGAXBridge.shared;
  __block NSDictionary *result = nil;
  __block NSError *err = nil;
  dispatch_sync(bridge.workQueue, ^{
    NSString *token = [bridge pushDevice:self.device];
    @try {
      int pid = 0;
      id element = [self frontmostElementWithBridge:bridge token:token pid:&pid error:&err];
      if (element) {
        NSRect frame = [(NSAccessibilityElement *)element accessibilityFrame];
        result = @{
          @"pid" : @(pid),
          @"frame" : @[ @(frame.origin.x), @(frame.origin.y), @(frame.size.width), @(frame.size.height) ],
          @"label" : AGStringValue([(NSAccessibilityElement *)element accessibilityLabel]) ?: @"",
        };
      }
    } @catch (NSException *e) {
      err = AGExceptionError(@"accessibility", e);
    } @finally {
      [bridge popToken:token];
    }
  });
  if (!result) {
    AG_FAIL(error, err ?: AGError(@"accessibility read failed"));
  }
  return result;
}

@end

#pragma mark - AGSimulators

@implementation AGSimulators

static id gDeviceSet;
static NSMutableDictionary<NSString *, AGDevice *> *gDeviceCache;

+ (id)deviceSetWithError:(NSError **)error
{
  if (!AGLoadFrameworks(error)) {
    return nil;
  }
  @synchronized(self) {
    if (gDeviceSet) {
      return gDeviceSet;
    }
    @try {
      NSError *err = nil;
      Class contextClass = NSClassFromString(@"SimServiceContext");
      id context = [(id<AGSimServiceContext>)contextClass sharedServiceContextForDeveloperDir:gDeveloperDir error:&err];
      if (!context) {
        AG_FAIL(error, AGWrap(@"SimServiceContext", err));
        return nil;
      }
      id set = [(id<AGSimServiceContext>)context defaultDeviceSetWithError:&err];
      if (!set) {
        AG_FAIL(error, AGWrap(@"defaultDeviceSet", err));
        return nil;
      }
      gDeviceSet = set;
      gDeviceCache = [NSMutableDictionary dictionary];
      return set;
    } @catch (NSException *e) {
      AG_FAIL(error, AGExceptionError(@"SimServiceContext", e));
      return nil;
    }
  }
}

+ (NSArray<AGDevice *> *)devicesWithError:(NSError **)error
{
  id set = [self deviceSetWithError:error];
  if (!set) {
    return nil;
  }
  @try {
    NSArray *simDevices = [(id<AGSimDeviceSet>)set devices];
    NSMutableArray<AGDevice *> *out = [NSMutableArray arrayWithCapacity:simDevices.count];
    @synchronized(self) {
      for (id<AGSimDevice> simDevice in simDevices) {
        NSString *udid = simDevice.UDID.UUIDString;
        AGDevice *device = gDeviceCache[udid];
        if (!device || device.device != simDevice) {
          device = [[AGDevice alloc] initWithDevice:simDevice];
          gDeviceCache[udid] = device;
        }
        [out addObject:device];
      }
    }
    return out;
  } @catch (NSException *e) {
    AG_FAIL(error, AGExceptionError(@"devices", e));
    return nil;
  }
}

+ (AGDevice *)deviceWithUDID:(NSString *)udid error:(NSError **)error
{
  @synchronized(self) {
    AGDevice *cached = gDeviceCache[udid.uppercaseString];
    if (cached) {
      return cached;
    }
  }
  NSArray<AGDevice *> *devices = [self devicesWithError:error];
  if (!devices) {
    return nil;
  }
  for (AGDevice *device in devices) {
    if ([device.udid caseInsensitiveCompare:udid] == NSOrderedSame) {
      return device;
    }
  }
  AG_FAIL(error, AGError(@"no simulator with UDID %@", udid));
  return nil;
}

@end
