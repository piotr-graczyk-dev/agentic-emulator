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

// One accessibility read (a tree, a frontmost probe, a hit test). Every request of the read
// is bounded: by the read's deadline and by a per-request timeout. A request that misses
// either is answered with an empty response and counted, so the read can fail with an error
// instead of returning a silently incomplete tree.
@interface AGAXSession : NSObject
@property (nonatomic, strong) id device;
@property (nonatomic, assign) CFAbsoluteTime deadline;
@property (nonatomic, assign) double requestTimeout;
@property (atomic, assign) NSUInteger requests;
@property (atomic, assign) NSUInteger timeouts;
@end

@implementation AGAXSession
@end

// AXPTranslator is a process-wide singleton with one token-delegate slot. Each read registers
// a token -> session mapping; the translator calls back with the token for every request, and
// we forward the request to that device via CoreSimulator.
@interface AGAXBridge : NSObject <AGAXPTranslationTokenDelegateHelper>
@property (nonatomic, strong, readonly) dispatch_queue_t callbackQueue;
@property (nonatomic, strong, readonly) dispatch_queue_t workQueue;
@property (nonatomic, strong, readonly) NSMutableDictionary<NSString *, AGAXSession *> *sessions;
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
  _sessions = [NSMutableDictionary dictionary];
  Class translatorClass = NSClassFromString(@"AXPTranslator");
  _translator = [(id<AGAXPTranslator>)translatorClass sharedInstance];
  [(id<AGAXPTranslator>)_translator setBridgeTokenDelegate:self];
  return self;
}

// timeout: the whole read's budget in seconds. Each request may take at most 5 s of it.
- (AGAXSession *)openSessionForDevice:(id)device timeout:(double)timeout token:(NSString **)token
{
  AGAXSession *session = [[AGAXSession alloc] init];
  session.device = device;
  if (timeout <= 0) {
    timeout = 20;
  }
  session.deadline = CFAbsoluteTimeGetCurrent() + timeout;
  session.requestTimeout = MIN(5.0, timeout);
  NSString *t = NSUUID.UUID.UUIDString;
  @synchronized(self.sessions) {
    self.sessions[t] = session;
  }
  *token = t;
  return session;
}

- (void)closeToken:(NSString *)token
{
  @synchronized(self.sessions) {
    [self.sessions removeObjectForKey:token];
  }
}

- (AGTranslationCallback)accessibilityTranslationDelegateBridgeCallbackWithToken:(NSString *)token
{
  AGAXSession *session;
  @synchronized(self.sessions) {
    session = self.sessions[token];
  }
  Class responseClass = NSClassFromString(@"AXPTranslatorResponse");
  if (!session) {
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
    session.requests += 1;
    double wait = session.requestTimeout;
    double remaining = session.deadline - CFAbsoluteTimeGetCurrent();
    if (remaining <= 0) {
      session.timeouts += 1;
      return [(id<AGAXPTranslatorResponse>)responseClass emptyResponse];
    }
    wait = MIN(wait, remaining);
    CFAbsoluteTime start = CFAbsoluteTimeGetCurrent();
    if (debug) {
      fprintf(stderr, "ax> %s\n", [[request description] UTF8String]);
    }
    dispatch_semaphore_t done = dispatch_semaphore_create(0);
    __block id response = nil;
    [(id<AGSimDevice>)session.device sendAccessibilityRequestAsync:request
                                                   completionQueue:queue
                                                 completionHandler:^(id inner) {
                                                   response = inner;
                                                   dispatch_semaphore_signal(done);
                                                 }];
    if (dispatch_semaphore_wait(done, dispatch_time(DISPATCH_TIME_NOW, (int64_t)(wait * NSEC_PER_SEC))) != 0) {
      session.timeouts += 1;
      return [(id<AGAXPTranslatorResponse>)responseClass emptyResponse];
    }
    if (!response) {
      return [(id<AGAXPTranslatorResponse>)responseClass emptyResponse];
    }
    if (debug) {
      NSString *d = [response description];
      fprintf(stderr, "ax< %.1f ms %s\n", (CFAbsoluteTimeGetCurrent() - start) * 1000,
              [[d substringToIndex:MIN((NSUInteger)200, d.length)] UTF8String]);
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

// Sends one translator request straight to the device (not through AXPTranslator), bounded
// by the session. Returns the response's resultData. Request types (AXPTranslatorRequest):
// 1 application by parameters[pid], 2 one attribute, 4 frontmost application, 5 multiple
// attributes, 6 hit test.
static id AGAXRequest(AGAXSession *session, id _Nullable translation, unsigned long long requestType,
                      unsigned long long attribute, NSDictionary *_Nullable parameters)
{
  Class requestClass = NSClassFromString(@"AXPTranslatorRequest");
  if (!requestClass) {
    return nil;
  }
  id request = [(id<AGAXPTranslatorRequest>)requestClass requestWithTranslation:translation];
  [request setRequestType:requestType];
  [request setAttributeType:attribute];
  if (parameters) {
    [request setParameters:parameters];
  }
  double wait = MIN(session.requestTimeout, session.deadline - CFAbsoluteTimeGetCurrent());
  session.requests += 1;
  if (wait <= 0) {
    session.timeouts += 1;
    return nil;
  }
  dispatch_semaphore_t done = dispatch_semaphore_create(0);
  __block id response = nil;
  [(id<AGSimDevice>)session.device sendAccessibilityRequestAsync:request
                                                 completionQueue:AGAXBridge.shared.callbackQueue
                                               completionHandler:^(id inner) {
                                                 response = inner;
                                                 dispatch_semaphore_signal(done);
                                               }];
  if (dispatch_semaphore_wait(done, dispatch_time(DISPATCH_TIME_NOW, (int64_t)(wait * NSEC_PER_SEC))) != 0) {
    session.timeouts += 1;
    return nil;
  }
  return [response respondsToSelector:@selector(resultData)] ? [response resultData] : nil;
}

static id AGAXDirect(AGAXSession *session, id translation, unsigned long long requestType,
                     unsigned long long attribute)
{
  return translation ? AGAXRequest(session, translation, requestType, attribute, nil) : nil;
}

// The application's bundle identifier (AXPAttributeBundleIdentifier = 6), one request.
static NSString *AGAXBundleIdentifier(AGAXSession *session, id element)
{
  id translation = [(id<AGAXPMacPlatformElement>)element translation];
  id value = AGAXDirect(session, translation, 2 /* attribute */, 6 /* AXPAttributeBundleIdentifier */);
  return [value isKindOfClass:NSString.class] ? value : nil;
}

// The application element of a process: request type 1 with parameters {pid}, one request
// (what idb's FBSimulatorAX sends for an explicit pid). nil when the process has none.
static id AGAXApplicationTranslation(AGAXSession *session, int pid)
{
  id translation = AGAXRequest(session, nil, 1, 0, @{@"pid" : @(pid)});
  if (![translation respondsToSelector:@selector(pid)] || [(id<AGAXPTranslationObject>)translation pid] != pid) {
    return nil;
  }
  return translation;
}

static NSError *AGAXTimeoutError(AGAXSession *session, NSString *what, double budget)
{
  return AGError(@"%@: the simulator's accessibility service did not answer in time (%lu of %lu requests "
                 @"timed out, budget %.1f s); the simulator is busy, try again",
                 what, (unsigned long)session.timeouts, (unsigned long)session.requests, budget);
}

// The attributes agemu maps to api.Element, fetched for one element in ONE translator request
// (AXPTranslatorRequest type 5: AXUIElementCopyMultipleAttributeValues inside the simulator)
// instead of one request per attribute.
static NSArray<NSString *> *AGAXAttributeNames(void)
{
  static NSArray *names;
  static dispatch_once_t once;
  dispatch_once(&once, ^{
    names = @[
      @"AXRole", @"AXSubrole", @"AXDescription", @"AXIdentifier", @"AXValue", @"AXFrame", @"AXEnabled", @"AXSelected",
      @"AXFocused", @"AXChildren", @"AXTitle", @"AXPlaceholderValue"
    ];
  });
  return names;
}

typedef struct {
  NSString *__unsafe_unretained token;
  AGAXSession *__unsafe_unretained session;
  NSInteger maxDepth;
  NSUInteger count;
  BOOL batched;
} AGAXWalk;

static NSDictionary *AGAXNode(id element, AGAXWalk *walk, NSInteger depth);

// iOS 26 collection views can be "opaque element providers" (AXPAttributeIsOpaqueElementProvider,
// 29): they report no AXChildren and expose their cells only through
// AXPAttributeVisibleOpaqueElements (58). The iPad Settings sidebar is one: AXChildren is empty
// (batched or not), its rows name it as their parent, and attribute 58 returns its 16 visible
// rows in one request. Hit tests found only the row on the sampled centre line.
static NSArray *AGAXOpaqueChildren(id element, AGAXWalk *walk, NSInteger depth)
{
  id translation = [(id<AGAXPMacPlatformElement>)element translation];
  id visible = AGAXDirect(walk->session, translation, 2 /* attribute */, 58 /* AXPAttributeVisibleOpaqueElements */);
  if (![visible isKindOfClass:NSArray.class]) {
    return @[];
  }
  id<AGAXPTranslator> translator = AGAXBridge.shared.translator;
  unsigned long long selfID = [(id<AGAXPTranslationObject>)translation objectID];
  NSMutableArray *out = [NSMutableArray arrayWithCapacity:[(NSArray *)visible count]];
  for (id child in (NSArray *)visible) {
    if (walk->session.timeouts > 0) {
      break;
    }
    if (![child respondsToSelector:@selector(objectID)] || [(id<AGAXPTranslationObject>)child objectID] == selfID) {
      continue;
    }
    [(id<AGAXPTranslationObject>)child setBridgeDelegateToken:walk->token];
    id childElement = [translator macPlatformElementFromTranslation:child];
    if (!childElement) {
      continue;
    }
    NSDictionary *node = AGAXNode(childElement, walk, depth);
    if (node) {
      [out addObject:node];
    }
  }
  return out;
}

// Some containers report no children although they have some (e.g. the iOS 26 Settings
// toolbar that hosts the search field: both AXChildren and AXChildrenInNavigationOrder come
// back empty). Hit-testing inside the container still finds them, so sample three points
// along its horizontal centre line and keep distinct elements that lie inside its frame.
static NSArray *AGAXHiddenChildren(NSRect frame, AGAXWalk *walk, NSInteger depth)
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
    id translation = [translator objectAtPoint:point displayId:0 bridgeDelegateToken:walk->token];
    if (!translation) {
      continue;
    }
    [(id<AGAXPTranslationObject>)translation setBridgeDelegateToken:walk->token];
    NSNumber *objectID = @([(id<AGAXPTranslationObject>)translation objectID]);
    if ([seen containsObject:objectID]) {
      continue;
    }
    [seen addObject:objectID];
    id element = [translator macPlatformElementFromTranslation:translation];
    if (!element) {
      continue;
    }
    [(id<AGAXPTranslationObject>)[(id<AGAXPMacPlatformElement>)element translation] setBridgeDelegateToken:walk->token];
    NSRect hit = [(NSAccessibilityElement *)element accessibilityFrame];
    if (NSIsEmptyRect(hit) || !NSContainsRect(bounds, hit) || NSEqualRects(hit, frame)) {
      continue;
    }
    NSDictionary *node = AGAXNode(element, walk, depth);
    if (node) {
      [out addObject:node];
    }
  }
  return out;
}

static BOOL AGAXBool(id value)
{
  return [value respondsToSelector:@selector(boolValue)] && [value boolValue];
}

// Reads one element: one batched request (plus the translator's own remote-element check),
// or one request per attribute when the translator cannot batch.
static NSDictionary *AGAXNode(id element, AGAXWalk *walk, NSInteger depth)
{
  [(id<AGAXPTranslationObject>)[(id<AGAXPMacPlatformElement>)element translation] setBridgeDelegateToken:walk->token];
  NSAccessibilityElement *e = element;
  NSMutableDictionary *node = [NSMutableDictionary dictionary];
  walk->count += 1;
  NSDictionary *v = nil;
  if (walk->batched) {
    id values = [(id<AGAXPMacPlatformElement>)element accessibilityMultipleAttributes:AGAXAttributeNames()];
    v = [values isKindOfClass:NSDictionary.class] ? values : @{};
  }
  NSRect frame = NSZeroRect;
  if (v) {
    NSValue *fv = v[@"AXFrame"];
    if ([fv isKindOfClass:NSValue.class]) {
      frame = fv.rectValue;
    }
  } else {
    frame = e.accessibilityFrame;
  }
  node[@"frame"] = @[ @(frame.origin.x), @(frame.origin.y), @(frame.size.width), @(frame.size.height) ];
  NSString *role = AGStringValue(v ? v[@"AXRole"] : e.accessibilityRole);
  if (role.length) {
    node[@"role"] = role;
  }
  NSString *subrole = AGStringValue(v ? v[@"AXSubrole"] : e.accessibilitySubrole);
  if (subrole.length) {
    node[@"subrole"] = subrole;
  }
  NSString *label = AGStringValue(v ? v[@"AXDescription"] : e.accessibilityLabel);
  if (label.length) {
    node[@"label"] = label;
  }
  NSString *identifier = AGStringValue(v ? v[@"AXIdentifier"] : e.accessibilityIdentifier);
  if (identifier.length) {
    node[@"id"] = identifier;
  }
  NSString *value = AGStringValue(v ? v[@"AXValue"] : e.accessibilityValue);
  if (value.length) {
    node[@"value"] = value;
  }
  if ([role isEqualToString:@"AXTextField"] || [role isEqualToString:@"AXTextArea"]) {
    // An empty field reports its placeholder as its value; the placeholder tells them apart.
    NSString *placeholder = AGStringValue(v ? v[@"AXPlaceholderValue"] : e.accessibilityPlaceholderValue);
    if (placeholder.length) {
      node[@"placeholder"] = placeholder;
    }
  }
  NSString *title = AGStringValue(v ? v[@"AXTitle"] : e.accessibilityTitle);
  if (title.length) {
    node[@"title"] = title;
  }
  // (BOOL) keeps it a JSON boolean: ?: on two BOOLs yields an int.
  node[@"enabled"] = @((BOOL)(v ? AGAXBool(v[@"AXEnabled"]) : e.isAccessibilityEnabled));
  if (v ? AGAXBool(v[@"AXSelected"]) : e.isAccessibilitySelected) {
    node[@"selected"] = @YES;
  }
  if (v ? AGAXBool(v[@"AXFocused"]) : e.isAccessibilityFocused) {
    node[@"focused"] = @YES;
  }
  if ((walk->maxDepth < 0 || depth < walk->maxDepth) && walk->session.timeouts == 0) {
    NSArray *children = v ? v[@"AXChildren"] : e.accessibilityChildren;
    if (![children isKindOfClass:NSArray.class]) {
      children = @[];
    }
    NSMutableArray *out = [NSMutableArray arrayWithCapacity:children.count];
    for (id child in children) {
      if (walk->session.timeouts > 0) {
        break;
      }
      NSDictionary *c = AGAXNode(child, walk, depth + 1);
      if (c) {
        [out addObject:c];
      }
    }
    if (out.count == 0 && depth > 0 && [role isEqualToString:@"AXGroup"] && walk->session.timeouts == 0) {
      // Sampling the centre line finds one row of content; a group taller than three 44 pt
      // rows can hold more, so ask it for its opaque elements first. Shorter ones (the iPhone
      // Settings toolbar, 86 pt) keep the centre-line sampling alone.
      if (NSHeight(frame) > 132) {
        [out addObjectsFromArray:AGAXOpaqueChildren(element, walk, depth + 1)];
      }
      if (out.count) {
        node[@"opaque"] = @YES;
      } else if (walk->session.timeouts == 0) {
        [out addObjectsFromArray:AGAXHiddenChildren(frame, walk, depth + 1)];
        if (out.count) {
          node[@"hitTested"] = @YES;
        }
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
// Display activity (see startDisplayMonitor): when the picture last changed.
@property (atomic, assign) CFAbsoluteTime lastFrameAt;
@property (atomic, assign) NSUInteger frameCount;
@property (atomic, assign) CFAbsoluteTime lastBigChangeAt; // last frame that changed > AGBigChange of the screen
@property (nonatomic, assign) BOOL monitorStarted;
@property (nonatomic, strong, readonly) NSUUID *callbackUUID;
// HID
@property (nonatomic, strong, readonly) dispatch_queue_t hidQueue;
@property (nonatomic, strong, readonly) dispatch_queue_t hidAckQueue;
@property (nonatomic, strong, nullable) id hidClient;
@property (nonatomic, strong) dispatch_group_t hidGroup;
@property (nonatomic, strong, nullable) NSError *hidDeliveryError;
// The boot the framebuffer / HID connections belong to (-[SimDevice lastBootedAt]).
@property (nonatomic, strong, nullable) NSDate *connectedBoot;
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
  [self resetConnections];
  @try {
    NSError *err = nil;
    // persist: keep the device booted after this process exits (as `simctl boot` does).
    NSDictionary *options = @{@"persist" : @YES};
    if (![self.device bootWithOptions:options error:&err]) {
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

- (NSDictionary *)bootStatusInfo
{
  @try {
    if (![self.device respondsToSelector:@selector(bootStatus)]) {
      return nil;
    }
    id info = [self.device bootStatus];
    if (!info || ![info respondsToSelector:@selector(status)] ||
        ![info respondsToSelector:@selector(isTerminalStatus)]) {
      return nil;
    }
    id<AGSimDeviceBootInfo> b = info;
    double elapsed = [info respondsToSelector:@selector(bootElapsedTime)] ? b.bootElapsedTime : 0;
    return @{@"status" : @(b.status), @"terminal" : @(b.isTerminalStatus), @"elapsed" : @(elapsed)};
  } @catch (NSException *e) {
    return nil;
  }
}

- (NSDate *)lastBootedAt
{
  return [self currentBoot];
}

- (NSDate *)currentBoot
{
  @try {
    if ([self.device respondsToSelector:@selector(lastBootedAt)]) {
      return [self.device lastBootedAt];
    }
  } @catch (NSException *e) {
  }
  return nil;
}

// A reboot by another tool (simctl, Xcode) kills the framebuffer and HID proxies without
// telling us: the HID client then fails with "device disconnected" and a cached surface
// would keep showing the last frame of the old boot. Reconnect when the boot changed.
- (void)reconnectIfRebooted
{
  NSDate *boot = [self currentBoot];
  BOOL stale = NO;
  @synchronized(self) {
    if (boot && self.connectedBoot && ![boot isEqualToDate:self.connectedBoot]) {
      stale = YES;
    }
  }
  if (stale) {
    [self resetConnections];
  }
}

- (void)noteConnected
{
  NSDate *boot = [self currentBoot];
  @synchronized(self) {
    if (boot) {
      self.connectedBoot = boot;
    }
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

// Sparse grid sampled from every new frame to measure how much of the screen changed.
static const NSUInteger AGGridW = 48, AGGridH = 104;
static const double AGBigChange = 0.004;

// Samples the grid from surface into cur and returns how many samples differ from prev.
static NSUInteger AGSampleGrid(IOSurfaceRef surface, uint32_t *cur, const uint32_t *prev)
{
  if (IOSurfaceLock(surface, kIOSurfaceLockReadOnly, NULL) != kIOReturnSuccess) {
    return AGGridW * AGGridH;
  }
  const uint8_t *base = IOSurfaceGetBaseAddress(surface);
  size_t bpr = IOSurfaceGetBytesPerRow(surface), w = IOSurfaceGetWidth(surface), h = IOSurfaceGetHeight(surface);
  size_t bpe = IOSurfaceGetBytesPerElement(surface);
  NSUInteger changed = 0;
  if (base && w > 0 && h > 0 && bpe == 4) {
    for (NSUInteger gy = 0; gy < AGGridH; gy++) {
      size_t y = (gy * 2 + 1) * h / (AGGridH * 2);
      const uint32_t *row = (const uint32_t *)(base + y * bpr);
      for (NSUInteger gx = 0; gx < AGGridW; gx++) {
        size_t x = (gx * 2 + 1) * w / (AGGridW * 2);
        uint32_t v = row[x];
        NSUInteger i = gy * AGGridW + gx;
        if (v != prev[i]) {
          changed++;
        }
        cur[i] = v;
      }
    }
  } else {
    changed = AGGridW * AGGridH;
  }
  IOSurfaceUnlock(surface, kIOSurfaceLockReadOnly, NULL);
  return changed;
}

// The display monitor samples the framebuffer surface's seed (bumped on every write, i.e. every
// new frame) every 8 ms on a background thread and records when the picture last changed, so
// "has the screen been still for N ms?" is answered without waiting. (The display's frame
// callback does not fire for this client.) Started when the framebuffer is first connected.
- (void)startDisplayMonitor
{
  @synchronized(self) {
    if (self.monitorStarted) {
      return;
    }
    self.monitorStarted = YES;
  }
  self.lastFrameAt = CFAbsoluteTimeGetCurrent();
  self.lastBigChangeAt = self.lastFrameAt;
  __weak AGDevice *weakSelf = self;
  [NSThread detachNewThreadWithBlock:^{
    IOSurface *last = nil;
    uint32_t lastSeed = 0;
    uint32_t *prev = calloc(AGGridW * AGGridH, sizeof(uint32_t));
    uint32_t *cur = calloc(AGGridW * AGGridH, sizeof(uint32_t));
    for (;;) {
      AGDevice *strong = weakSelf;
      if (!strong) {
        return;
      }
      IOSurface *current;
      @synchronized(strong) {
        current = strong.surface;
      }
      if (!current) {
        strong = nil;
        usleep(50000);
        continue;
      }
      uint32_t seed = IOSurfaceGetSeed((__bridge IOSurfaceRef)current);
      if (current != last || seed != lastSeed) {
        BOOL newSurface = current != last;
        last = current;
        lastSeed = seed;
        CFAbsoluteTime now = CFAbsoluteTimeGetCurrent();
        strong.lastFrameAt = now;
        strong.frameCount += 1;
        // A change counts as big when more than AGBigChange of a sparse grid of pixels differ:
        // scrolling and screen transitions are big; a blinking cursor, a spinner or a ticking
        // countdown are not, so waiting for "no big change" ends while they keep running.
        NSUInteger changed = AGSampleGrid((__bridge IOSurfaceRef)current, cur, prev);
        if (newSurface || changed > (NSUInteger)(AGGridW * AGGridH * AGBigChange)) {
          strong.lastBigChangeAt = now;
        }
        uint32_t *t = prev;
        prev = cur;
        cur = t;
      }
      strong = nil;
      usleep(8000);
    }
  }];
}

// Waits until the display has shown no new frame for quietMs (an animation or transition has
// ended), at most maxMs.
- (NSDictionary *)waitForSettleQuiet:(double)quietMs max:(double)maxMs
{
  NSError *err = nil;
  if (![self framebufferWithError:&err]) {
    return @{@"settled" : @NO, @"waitedMs" : @0, @"frames" : @0, @"error" : err.localizedDescription ?: @"no framebuffer"};
  }
  [self startDisplayMonitor];
  CFAbsoluteTime start = CFAbsoluteTimeGetCurrent();
  NSUInteger framesBefore = self.frameCount;
  double quiet = quietMs / 1000, limit = maxMs / 1000;
  for (;;) {
    CFAbsoluteTime now = CFAbsoluteTimeGetCurrent();
    BOOL settled = now - self.lastFrameAt >= quiet;
    if (settled || now - start >= limit) {
      return @{
        @"settled" : @(settled),
        @"waitedMs" : @((now - start) * 1000),
        @"frames" : @(self.frameCount - framesBefore),
      };
    }
    usleep(4000);
  }
}

- (NSDictionary *)waitForStillQuiet:(double)quietMs max:(double)maxMs
{
  NSError *err = nil;
  if (![self framebufferWithError:&err]) {
    return @{@"settled" : @NO, @"waitedMs" : @0, @"error" : err.localizedDescription ?: @"no framebuffer"};
  }
  [self startDisplayMonitor];
  CFAbsoluteTime start = CFAbsoluteTimeGetCurrent();
  double quiet = quietMs / 1000, limit = maxMs / 1000;
  for (;;) {
    CFAbsoluteTime now = CFAbsoluteTimeGetCurrent();
    BOOL settled = now - self.lastBigChangeAt >= quiet;
    if (settled || now - start >= limit) {
      return @{@"settled" : @(settled), @"waitedMs" : @((now - start) * 1000)};
    }
    usleep(4000);
  }
}

- (NSDictionary *)waitForFrameSince:(NSUInteger)mark quiet:(double)quietMs max:(double)maxMs
{
  NSError *err = nil;
  if (![self framebufferWithError:&err]) {
    return @{@"mark" : @0, @"error" : err.localizedDescription ?: @"no framebuffer"};
  }
  [self startDisplayMonitor];
  CFAbsoluteTime start = CFAbsoluteTimeGetCurrent();
  double quiet = quietMs / 1000, limit = maxMs / 1000;
  while (self.frameCount <= mark && CFAbsoluteTimeGetCurrent() - start < limit) {
    usleep(2000);
  }
  while (CFAbsoluteTimeGetCurrent() - self.lastBigChangeAt < quiet && CFAbsoluteTimeGetCurrent() - start < limit) {
    usleep(2000);
  }
  return @{@"mark" : @(self.frameCount), @"waitedMs" : @((CFAbsoluteTimeGetCurrent() - start) * 1000)};
}

- (NSUInteger)inputFrameMark
{
  NSError *err = nil;
  if (![self framebufferWithError:&err]) {
    return NSUIntegerMax;
  }
  [self startDisplayMonitor];
  return self.frameCount;
}

- (NSDictionary *)waitForChangeSince:(NSUInteger)mark quiet:(double)quietMs max:(double)maxMs
{
  if (mark == NSUIntegerMax) {
    return @{@"changed" : @NO, @"waitedMs" : @0};
  }
  CFAbsoluteTime start = CFAbsoluteTimeGetCurrent();
  double quiet = quietMs / 1000, limit = maxMs / 1000;
  while (self.frameCount == mark) {
    if (CFAbsoluteTimeGetCurrent() - start >= limit) {
      return @{@"changed" : @NO, @"waitedMs" : @((CFAbsoluteTimeGetCurrent() - start) * 1000)};
    }
    usleep(2000);
  }
  for (;;) {
    CFAbsoluteTime now = CFAbsoluteTimeGetCurrent();
    BOOL settled = now - self.lastFrameAt >= quiet;
    if (settled || now - start >= limit) {
      return @{@"changed" : @YES, @"settled" : @(settled), @"waitedMs" : @((now - start) * 1000)};
    }
    usleep(4000);
  }
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

- (BOOL)framebufferConnected
{
  [self reconnectIfRebooted];
  @synchronized(self) {
    return self.surface != nil;
  }
}

- (BOOL)hidConnected
{
  [self reconnectIfRebooted];
  __block BOOL connected = NO;
  dispatch_sync(self.hidQueue, ^{
    connected = self.hidClient != nil;
  });
  return connected;
}

- (IOSurface *)framebufferWithError:(NSError **)error
{
  [self reconnectIfRebooted];
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
      [self noteConnected];
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
    [self noteConnected];
    return YES;
  } @catch (NSException *e) {
    AG_FAIL(error, AGExceptionError(@"SimDeviceLegacyHIDClient init", e));
    return NO;
  }
}

- (BOOL)prepareHIDWithError:(NSError **)error
{
  if (!AGLoadFrameworks(error)) {
    return NO;
  }
  [self reconnectIfRebooted];
  __block BOOL ok = NO;
  __block NSError *err = nil;
  dispatch_sync(self.hidQueue, ^{
    ok = [self ensureHIDClient:&err];
  });
  if (!ok) {
    AG_FAIL(error, err);
  }
  return ok;
}

// Hands a malloc'ed Indigo message to the HID client, which frees it.
- (BOOL)sendIndigoMessage:(void *)message error:(NSError **)error
{
  if (!message) {
    AG_FAIL(error, AGError(@"Indigo message builder returned NULL"));
    return NO;
  }
  [self reconnectIfRebooted];
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
    // The client is bound to a dead connection (e.g. the device rebooted): reconnect next time.
    dispatch_sync(self.hidQueue, ^{
      self.hidClient = nil;
    });
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

static BOOL AGAXBatching(id element);

- (BOOL)isPad
{
  return [self.deviceTypeIdentifier containsString:@"iPad"];
}

// The application element of the process that owns the screen, on iPad.
//
// The bridge's frontmost application is the first of SpringBoard's focused applications (iOS
// attribute 1102). On iPad (iOS 26.5) that list starts with the dock's
// com.apple.DocumentManager.DockFolderViewService on the home screen and for about a second
// after an app comes to the front, so its empty tree was read instead of the screen's. A
// system-wide hit test (request type 6) is answered by the process whose window is at that
// point: SpringBoard on the home screen, Settings while Settings is in front.
//
// So hit-test the screen centre. When the centre belongs to the reported frontmost process,
// keep it (2 requests in all). Otherwise take the hit process's application element (request
// type 1) if it covers at least 40% of the screen; that rejects hosted content such as a home
// screen widget (WidgetRenderer, 260x260 pt), and then four more points are tried. Nothing found (right after a launch every hit test answers nothing for about a
// second): the reported frontmost application, as before, with *confirmed set to NO.
- (id)screenOwnerForFrontmost:(id)frontmost
                        token:(NSString *)token
                      session:(AGAXSession *)session
                    confirmed:(BOOL *)confirmed
{
  *confirmed = NO;
  id<AGAXPTranslator> translator = AGAXBridge.shared.translator;
  double scale = self.screenScale > 0 ? self.screenScale : 1;
  CGSize pixels = self.screenPixels;
  NSRect screen = NSMakeRect(0, 0, pixels.width / scale, pixels.height / scale);
  if (screen.size.width < 1 || screen.size.height < 1) {
    return frontmost;
  }
  int frontPid = [(id<AGAXPTranslationObject>)frontmost pid];
  NSMutableSet<NSNumber *> *rejected = [NSMutableSet set];
  const double points[5][2] = {{0.5, 0.5}, {0.5, 0.3}, {0.5, 0.7}, {0.3, 0.5}, {0.7, 0.5}};
  for (int i = 0; i < 5 && session.timeouts == 0; i++) {
    CGPoint p = CGPointMake(points[i][0] * NSWidth(screen), points[i][1] * NSHeight(screen));
    id hit = [translator objectAtPoint:p displayId:0 bridgeDelegateToken:token];
    int pid = hit ? [(id<AGAXPTranslationObject>)hit pid] : 0;
    if (pid <= 0 || [rejected containsObject:@(pid)]) {
      continue;
    }
    if (pid == frontPid) {
      *confirmed = YES;
      return frontmost;
    }
    id app = AGAXApplicationTranslation(session, pid);
    if (app) {
      [(id<AGAXPTranslationObject>)app setBridgeDelegateToken:token];
      id element = [translator macPlatformElementFromTranslation:app];
      [(id<AGAXPTranslationObject>)[(id<AGAXPMacPlatformElement>)element translation] setBridgeDelegateToken:token];
      NSRect frame = NSZeroRect;
      if (AGAXBatching(element)) {
        NSDictionary *v = [(id<AGAXPMacPlatformElement>)element accessibilityMultipleAttributes:@[ @"AXFrame" ]];
        NSValue *fv = [v isKindOfClass:NSDictionary.class] ? v[@"AXFrame"] : nil;
        frame = [fv isKindOfClass:NSValue.class] ? fv.rectValue : NSZeroRect;
      } else if (element) {
        frame = [(NSAccessibilityElement *)element accessibilityFrame];
      }
      NSRect covered = NSIntersectionRect(frame, screen);
      if (NSWidth(covered) * NSHeight(covered) >= 0.4 * NSWidth(screen) * NSHeight(screen)) {
        *confirmed = YES;
        return app;
      }
    }
    [rejected addObject:@(pid)];
  }
  return frontmost;
}

// The frontmost application's element. *confirmed is NO when, on iPad, no hit test found the
// process that owns the screen (see screenOwnerForFrontmost:).
- (id)frontmostElementWithBridge:(AGAXBridge *)bridge
                           token:(NSString *)token
                         session:(AGAXSession *)session
                             pid:(int *)pid
                       confirmed:(BOOL *)confirmed
                           error:(NSError **)error
{
  *confirmed = YES;
  id<AGAXPTranslator> translator = bridge.translator;
  id translation = [translator frontmostApplicationWithDisplayId:0 bridgeDelegateToken:token];
  if (!translation) {
    AG_FAIL(error, AGError(@"no frontmost application (is SpringBoard running?)"));
    return nil;
  }
  [(id<AGAXPTranslationObject>)translation setBridgeDelegateToken:token];
  if (self.isPad) {
    translation = [self screenOwnerForFrontmost:translation token:token session:session confirmed:confirmed];
  }
  *pid = [(id<AGAXPTranslationObject>)translation pid];
  id element = [translator macPlatformElementFromTranslation:translation];
  if (!element) {
    AG_FAIL(error, AGError(@"no accessibility element for the frontmost application"));
    return nil;
  }
  [(id<AGAXPTranslationObject>)[(id<AGAXPMacPlatformElement>)element translation] setBridgeDelegateToken:token];
  return element;
}

static BOOL AGAXBatching(id element)
{
  static BOOL disabled;
  static dispatch_once_t once;
  dispatch_once(&once, ^{
    disabled = getenv("AGEMU_AX_UNBATCHED") != NULL;
  });
  return !disabled && [element respondsToSelector:@selector(accessibilityMultipleAttributes:)];
}

// Runs body on the accessibility work queue with a session bounded by timeout (seconds).
- (nullable id)withAXSession:(double)timeout
                        what:(NSString *)what
                       error:(NSError **)error
                        body:(id _Nullable (^)(AGAXBridge *bridge, NSString *token, AGAXSession *session,
                                               NSError **err))body
{
  if (!AGLoadFrameworks(error)) {
    return nil;
  }
  if (self.device.state != 3) {
    AG_FAIL(error, AGError(@"device %@ is not booted (%@)", self.udid, self.stateString));
    return nil;
  }
  AGAXBridge *bridge = AGAXBridge.shared;
  __block id result = nil;
  __block NSError *err = nil;
  double budget = timeout > 0 ? timeout : 20;
  CFAbsoluteTime enqueued = CFAbsoluteTimeGetCurrent();
  dispatch_sync(bridge.workQueue, ^{
    // Time spent queued behind another read counts against this read's budget.
    double left = budget - (CFAbsoluteTimeGetCurrent() - enqueued);
    if (left <= 0) {
      err = AGError(@"%@: waited %.1f s for another accessibility read to finish", what, budget);
      return;
    }
    NSString *token = nil;
    AGAXSession *session = [bridge openSessionForDevice:self.device timeout:left token:&token];
    @try {
      NSError *inner = nil;
      result = body(bridge, token, session, &inner);
      if (session.timeouts > 0) {
        result = nil;
        err = AGAXTimeoutError(session, what, budget);
      } else if (!result) {
        err = inner;
      }
    } @catch (NSException *e) {
      result = nil;
      err = AGExceptionError(what, e);
    } @finally {
      [bridge closeToken:token];
    }
  });
  if (!result) {
    AG_FAIL(error, err ?: AGError(@"%@ failed", what));
  }
  return result;
}

- (NSDictionary *)accessibilityTreeWithMaxDepth:(NSInteger)maxDepth timeout:(double)timeout error:(NSError **)error
{
  return [self withAXSession:timeout
                        what:@"accessibility tree"
                       error:error
                        body:^id(AGAXBridge *bridge, NSString *token, AGAXSession *session, NSError **err) {
                          int pid = 0;
                          BOOL confirmed = YES;
                          id element = [self frontmostElementWithBridge:bridge
                                                                  token:token
                                                                session:session
                                                                    pid:&pid
                                                              confirmed:&confirmed
                                                                  error:err];
                          if (!element) {
                            return nil;
                          }
                          AGAXWalk walk = {
                            .token = token, .session = session, .maxDepth = maxDepth, .count = 0,
                            .batched = AGAXBatching(element)};
                          NSString *bundle = AGAXBundleIdentifier(session, element);
                          NSDictionary *root = AGAXNode(element, &walk, 0);
                          return @{
                            @"pid" : @(pid),
                            @"bundleId" : bundle ?: @"",
                            @"root" : root,
                            @"elements" : @(walk.count),
                            @"requests" : @(session.requests),
                            @"batched" : @(walk.batched),
                            @"unconfirmed" : @(!confirmed),
                          };
                        }];
}

- (NSDictionary *)accessibilityAtPointX:(double)x
                                      y:(double)y
                               maxDepth:(NSInteger)maxDepth
                                timeout:(double)timeout
                                  error:(NSError **)error
{
  return [self withAXSession:timeout
                        what:@"accessibility hit test"
                       error:error
                        body:^id(AGAXBridge *bridge, NSString *token, AGAXSession *session, NSError **err) {
                          id<AGAXPTranslator> translator = bridge.translator;
                          id translation = [translator objectAtPoint:CGPointMake(x, y) displayId:0 bridgeDelegateToken:token];
                          if (!translation) {
                            AG_FAIL(err, AGError(@"nothing at %.0f,%.0f", x, y));
                            return nil;
                          }
                          [(id<AGAXPTranslationObject>)translation setBridgeDelegateToken:token];
                          id element = [translator macPlatformElementFromTranslation:translation];
                          [(id<AGAXPTranslationObject>)[(id<AGAXPMacPlatformElement>)element translation]
                            setBridgeDelegateToken:token];
                          AGAXWalk walk = {
                            .token = token, .session = session, .maxDepth = maxDepth, .count = 0,
                            .batched = AGAXBatching(element)};
                          NSMutableDictionary *node = [AGAXNode(element, &walk, 0) mutableCopy];
                          NSArray *names = [(NSAccessibilityElement *)element accessibilityAttributeNames];
                          node[@"attributeNames"] = names ?: @[];
                          id parent = [(NSAccessibilityElement *)element accessibilityParent];
                          if (parent) {
                            node[@"parentRole"] = AGStringValue([(NSAccessibilityElement *)parent accessibilityRole]) ?: @"";
                          }
                          return node;
                        }];
}

- (NSDictionary *)accessibilityFrontmostWithTimeout:(double)timeout full:(BOOL)full error:(NSError **)error
{
  return [self withAXSession:timeout
                        what:@"frontmost application"
                       error:error
                        body:^id(AGAXBridge *bridge, NSString *token, AGAXSession *session, NSError **err) {
                          int pid = 0;
                          BOOL confirmed = YES;
                          id element = [self frontmostElementWithBridge:bridge
                                                                  token:token
                                                                session:session
                                                                    pid:&pid
                                                              confirmed:&confirmed
                                                                  error:err];
                          if (!element) {
                            return nil;
                          }
                          if (!full) {
                            return @{@"pid" : @(pid), @"unconfirmed" : @(!confirmed)};
                          }
                          NSString *bundle = AGAXBundleIdentifier(session, element);
                          NSRect frame = NSZeroRect;
                          NSString *label = nil;
                          if (AGAXBatching(element)) {
                            NSDictionary *v = [(id<AGAXPMacPlatformElement>)element
                              accessibilityMultipleAttributes:@[ @"AXFrame", @"AXDescription" ]];
                            if ([v isKindOfClass:NSDictionary.class]) {
                              NSValue *fv = v[@"AXFrame"];
                              frame = [fv isKindOfClass:NSValue.class] ? fv.rectValue : NSZeroRect;
                              label = AGStringValue(v[@"AXDescription"]);
                            }
                          } else {
                            frame = [(NSAccessibilityElement *)element accessibilityFrame];
                            label = AGStringValue([(NSAccessibilityElement *)element accessibilityLabel]);
                          }
                          return @{
                            @"pid" : @(pid),
                            @"frame" : @[ @(frame.origin.x), @(frame.origin.y), @(frame.size.width), @(frame.size.height) ],
                            @"label" : label ?: @"",
                            @"bundleId" : bundle ?: @"",
                            @"unconfirmed" : @(!confirmed),
                          };
                        }];
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
