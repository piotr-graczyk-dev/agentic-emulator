// AgemuSim: test capabilities (docs/CAPABILITIES.md). These are CoreSimulator's own SimDevice
// calls, the ones `simctl openurl / privacy / location / ui appearance / push / pbcopy /
// pbpaste` make, issued from the long-lived helper instead of one simctl process per call.
//
// The pasteboard goes through SimPasteboardPlus (what simctl pbcopy/pbpaste use): a
// SimPasteboardInterface connected to the device's pasteboard service mirrors it into a private
// NSPasteboard of the helper (never the Mac's general pasteboard, so the Mac clipboard cannot
// reach the device). The connection is kept per device and boot.
#import "AgemuSim.h"

#import <AppKit/AppKit.h>
#import <dlfcn.h>

// Implemented in AgemuSim.m (its class extension).
@interface AGDevice (AGCapabilitiesAccess)
- (id)device;
@end

@protocol AGSimDeviceCapabilities <NSObject>
- (BOOL)openURL:(NSURL *)url error:(NSError **)error;
- (BOOL)setPrivacyAccessForService:(NSString *)service bundleID:(NSString *)bundleID granted:(BOOL)granted error:(NSError **)error;
- (BOOL)resetPrivacyAccessForService:(NSString *)service bundleID:(nullable NSString *)bundleID error:(NSError **)error;
- (BOOL)setLocationWithLatitude:(double)latitude andLongitude:(double)longitude error:(NSError **)error;
- (BOOL)clearSimulatedLocationWithError:(NSError **)error;
- (long long)currentUIInterfaceStyle;
- (BOOL)setUIInterfaceStyle:(long long)style error:(NSError **)error;
- (BOOL)sendPushNotificationForBundleID:(NSString *)bundleID jsonPayload:(NSDictionary *)payload error:(NSError **)error;
- (nullable NSDictionary *)propertiesOfApplication:(NSString *)bundleID error:(NSError **)error;
- (unsigned int)lookup:(NSString *)service error:(NSError **)error;
- (nullable NSDate *)lastBootedAt;
@end

// SimPasteboardPlus.SimPasteboardInterface (a Swift class exported to Objective-C).
@protocol AGSimPasteboardInterface <NSObject>
- (instancetype)initWithConnectingToPort:(unsigned int)port
                      managingPasteboard:(NSPasteboard *)pasteboard
                                delegate:(id)delegate
                           delegateQueue:(dispatch_queue_t)queue;
- (void)push;
- (void)pull;
@end

static NSError *AGCapError(NSString *format, ...) NS_FORMAT_FUNCTION(1, 2);
static NSError *AGCapError(NSString *format, ...)
{
  va_list ap;
  va_start(ap, format);
  NSString *message = [[NSString alloc] initWithFormat:format arguments:ap];
  va_end(ap);
  return [NSError errorWithDomain:AGErrorDomain code:1 userInfo:@{NSLocalizedDescriptionKey : message}];
}

// CoreSimulator's errors wrap the device's reason ("Failed to set access", "LSApplicationWorkspace
// error 115") under a generic "Simulator device failed to ..." message: keep both.
static NSError *AGCapWrap(NSString *what, NSError *_Nullable err)
{
  if (!err) {
    return AGCapError(@"%@ failed", what);
  }
  NSError *under = err.userInfo[NSUnderlyingErrorKey];
  NSString *detail = under.localizedDescription;
  if (detail.length && ![detail isEqualToString:err.localizedDescription] && ![detail isEqualToString:@"(null)"]) {
    return AGCapError(@"%@: %@ (%@ %ld: %@)", what, err.localizedDescription, under.domain, (long)under.code, detail);
  }
  return AGCapError(@"%@: %@ (%@ %ld)", what, err.localizedDescription, err.domain, (long)err.code);
}

#define AG_CAP_FAIL(errp, e)                                                                                           \
  do {                                                                                                                 \
    if (errp) {                                                                                                        \
      *(errp) = (e);                                                                                                   \
    }                                                                                                                  \
  } while (0)

// Runs one CoreSimulator call that reports failure as NO + NSError, catching exceptions.
static BOOL AGCapCall(NSString *what, NSError **error, BOOL (^body)(NSError **err))
{
  @try {
    NSError *err = nil;
    if (!body(&err)) {
      AG_CAP_FAIL(error, AGCapWrap(what, err));
      return NO;
    }
    return YES;
  } @catch (NSException *e) {
    AG_CAP_FAIL(error, AGCapError(@"%@ raised %@: %@", what, e.name, e.reason));
    return NO;
  }
}

#pragma mark - Pasteboard link

// One SimPasteboardInterface connection to a device's pasteboard, for one boot. pull asks the
// device for its current pasteboard; the delegate is told when that snapshot has been applied to
// the private NSPasteboard. push sends the private NSPasteboard to the device.
@interface AGPasteboardLink : NSObject
@property (nonatomic, strong) id<AGSimPasteboardInterface> iface;
@property (nonatomic, strong) NSPasteboard *pasteboard;
@property (nonatomic, strong) NSDate *boot;
@property (nonatomic, strong) dispatch_queue_t queue;
@property (nonatomic, strong) NSCondition *cond;
@property (nonatomic, assign) NSUInteger applied; // snapshots applied (or failed) so far
@property (nonatomic, strong, nullable) NSError *applyError;
@property (atomic, assign) BOOL dead;
@end

@implementation AGPasteboardLink

- (void)simPasteboardDidBecomeActive:(id)iface
{
}

- (void)simPasteboardDidLoseConnection:(id)iface
{
  self.dead = YES;
  [self.cond lock];
  [self.cond broadcast];
  [self.cond unlock];
}

- (BOOL)simPasteboardShouldApplySnapshot:(id)iface fromHost:(NSUUID *)host generation:(long long)generation toPasteboard:(NSPasteboard *)pasteboard
{
  return YES;
}

- (void)simPasteboardDidApplySnapshot:(id)iface
                             fromHost:(NSUUID *)host
                   incomingGeneration:(long long)generation
                         toPasteboard:(NSPasteboard *)pasteboard
                         atGeneration:(long long)local
{
  [self.cond lock];
  self.applied++;
  self.applyError = nil;
  [self.cond broadcast];
  [self.cond unlock];
}

- (void)simPasteboardErrorApplyingSnapshot:(id)iface
                                  fromHost:(NSUUID *)host
                                generation:(long long)generation
                                     error:(NSError *)error
                              toPasteboard:(NSPasteboard *)pasteboard
{
  [self.cond lock];
  self.applied++;
  self.applyError = error;
  [self.cond broadcast];
  [self.cond unlock];
}

- (void)simPasteboardDidChangeAutoNotifyState:(id)iface forPasteboard:(NSPasteboard *)pasteboard toStatus:(BOOL)status
{
}

// Pulls the device's pasteboard into the private one and waits until it is applied.
- (BOOL)pull:(NSTimeInterval)timeout error:(NSError **)error
{
  [self.cond lock];
  NSUInteger before = self.applied;
  [self.cond unlock];
  [self.iface pull];
  NSDate *deadline = [NSDate dateWithTimeIntervalSinceNow:timeout];
  [self.cond lock];
  while (self.applied == before && !self.dead) {
    if (![self.cond waitUntilDate:deadline]) {
      break;
    }
  }
  BOOL got = self.applied != before;
  NSError *applyError = self.applyError;
  [self.cond unlock];
  if (self.dead) {
    AG_CAP_FAIL(error, AGCapError(@"the device's pasteboard connection was lost"));
    return NO;
  }
  if (!got) {
    self.dead = YES; // do not reuse a connection that stopped answering
    AG_CAP_FAIL(error, AGCapError(@"the device's pasteboard did not answer within %.0f s", timeout));
    return NO;
  }
  if (applyError) {
    AG_CAP_FAIL(error, AGCapWrap(@"reading the device's pasteboard", applyError));
    return NO;
  }
  return YES;
}

- (void)close
{
  self.dead = YES;
  [self.pasteboard releaseGlobally];
}

@end

static NSMutableDictionary<NSString *, AGPasteboardLink *> *gPasteboardLinks;
static NSMutableDictionary<NSString *, NSObject *> *gPasteboardLocks;

#pragma mark - AGDevice (capabilities)

@implementation AGDevice (AGCapabilities)

- (id<AGSimDeviceCapabilities>)capDevice
{
  return (id<AGSimDeviceCapabilities>)[self device];
}

- (BOOL)openURL:(NSString *)url error:(NSError **)error
{
  NSURL *u = [NSURL URLWithString:url];
  if (!u) {
    AG_CAP_FAIL(error, AGCapError(@"not a URL: %@", url));
    return NO;
  }
  return AGCapCall([NSString stringWithFormat:@"open %@", url], error, ^BOOL(NSError **err) {
    return [self.capDevice openURL:u error:err];
  });
}

- (BOOL)setPrivacy:(NSString *)service bundleID:(NSString *)bundleID granted:(BOOL)granted error:(NSError **)error
{
  NSString *what = [NSString stringWithFormat:@"%@ %@ for %@", granted ? @"grant" : @"revoke", service, bundleID];
  return AGCapCall(what, error, ^BOOL(NSError **err) {
    return [self.capDevice setPrivacyAccessForService:service bundleID:bundleID granted:granted error:err];
  });
}

- (BOOL)resetPrivacy:(NSString *)service bundleID:(NSString *)bundleID error:(NSError **)error
{
  NSString *what = [NSString stringWithFormat:@"reset %@ for %@", service, bundleID ?: @"every app"];
  return AGCapCall(what, error, ^BOOL(NSError **err) {
    return [self.capDevice resetPrivacyAccessForService:service bundleID:bundleID error:err];
  });
}

- (BOOL)setLocationLatitude:(double)latitude longitude:(double)longitude error:(NSError **)error
{
  return AGCapCall(@"set location", error, ^BOOL(NSError **err) {
    return [self.capDevice setLocationWithLatitude:latitude andLongitude:longitude error:err];
  });
}

- (BOOL)clearLocationWithError:(NSError **)error
{
  return AGCapCall(@"clear location", error, ^BOOL(NSError **err) {
    return [self.capDevice clearSimulatedLocationWithError:err];
  });
}

- (NSInteger)interfaceStyle
{
  @try {
    return (NSInteger)[self.capDevice currentUIInterfaceStyle];
  } @catch (NSException *e) {
    return 0;
  }
}

- (BOOL)setInterfaceStyle:(NSInteger)style error:(NSError **)error
{
  return AGCapCall(@"set appearance", error, ^BOOL(NSError **err) {
    return [self.capDevice setUIInterfaceStyle:style error:err];
  });
}

- (BOOL)sendPush:(NSDictionary *)payload bundleID:(NSString *)bundleID error:(NSError **)error
{
  return AGCapCall([NSString stringWithFormat:@"push to %@", bundleID], error, ^BOOL(NSError **err) {
    return [self.capDevice sendPushNotificationForBundleID:bundleID jsonPayload:payload error:err];
  });
}

- (NSDictionary *)applicationProperties:(NSString *)bundleID error:(NSError **)error
{
  @try {
    NSError *err = nil;
    NSDictionary *props = [self.capDevice propertiesOfApplication:bundleID error:&err];
    if (!props) {
      AG_CAP_FAIL(error, AGCapWrap([NSString stringWithFormat:@"%@ is not installed", bundleID], err));
    }
    return props;
  } @catch (NSException *e) {
    AG_CAP_FAIL(error, AGCapError(@"application properties raised %@: %@", e.name, e.reason));
    return nil;
  }
}

#pragma mark Pasteboard

// Serializes pasteboard use per device: one pull or push at a time.
- (NSObject *)pasteboardLock
{
  @synchronized([AGDevice class]) {
    if (!gPasteboardLocks) {
      gPasteboardLocks = [NSMutableDictionary dictionary];
    }
    NSObject *l = gPasteboardLocks[self.udid];
    if (!l) {
      l = [NSObject new];
      gPasteboardLocks[self.udid] = l;
    }
    return l;
  }
}

// The connection for the current boot (the caller holds pasteboardLock).
- (AGPasteboardLink *)pasteboardLinkWithError:(NSError **)error
{
  if (!gPasteboardLinks) {
    gPasteboardLinks = [NSMutableDictionary dictionary];
  }
  NSDate *boot = [self.capDevice lastBootedAt];
  AGPasteboardLink *link = gPasteboardLinks[self.udid];
  if (link && !link.dead && (!boot || [boot isEqualToDate:link.boot])) {
    return link;
  }
  if (link) {
    [link close];
    [gPasteboardLinks removeObjectForKey:self.udid];
  }
  if (self.state != 3) {
    AG_CAP_FAIL(error, AGCapError(@"the device must be booted to use its pasteboard"));
    return nil;
  }
  static Class interfaceClass;
  static dispatch_once_t once;
  dispatch_once(&once, ^{
    dlopen("/Library/Developer/PrivateFrameworks/CoreSimulator.framework/Versions/A/Frameworks/SimPasteboardPlus.framework/"
           "Versions/A/SimPasteboardPlus",
           RTLD_NOW);
    interfaceClass = NSClassFromString(@"_TtC17SimPasteboardPlus22SimPasteboardInterface");
  });
  if (!interfaceClass) {
    AG_CAP_FAIL(error, AGCapError(@"SimPasteboardPlus (SimPasteboardInterface) is not available in this Xcode"));
    return nil;
  }
  NSError *err = nil;
  unsigned int port = 0;
  @try {
    port = [self.capDevice lookup:@"com.apple.CoreSimulator.SimPasteboardInterface" error:&err];
  } @catch (NSException *e) {
    AG_CAP_FAIL(error, AGCapError(@"pasteboard service lookup raised %@: %@", e.name, e.reason));
    return nil;
  }
  if (!port) {
    AG_CAP_FAIL(error, AGCapWrap(@"looking up the device's pasteboard service", err));
    return nil;
  }
  link = [AGPasteboardLink new];
  link.boot = boot;
  link.cond = [NSCondition new];
  link.queue = dispatch_queue_create("agemu.pasteboard", DISPATCH_QUEUE_SERIAL);
  link.pasteboard = [NSPasteboard pasteboardWithUniqueName];
  @try {
    link.iface = [(id<AGSimPasteboardInterface>)[interfaceClass alloc] initWithConnectingToPort:port
                                                                             managingPasteboard:link.pasteboard
                                                                                       delegate:link
                                                                                  delegateQueue:link.queue];
  } @catch (NSException *e) {
    [link close];
    AG_CAP_FAIL(error, AGCapError(@"connecting to the device's pasteboard raised %@: %@", e.name, e.reason));
    return nil;
  }
  if (!link.iface) {
    [link close];
    AG_CAP_FAIL(error, AGCapError(@"connecting to the device's pasteboard failed"));
    return nil;
  }
  gPasteboardLinks[self.udid] = link;
  return link;
}

// Runs body with a live link; a connection that turns out dead is replaced once.
- (BOOL)withPasteboard:(NSError **)error body:(BOOL (^)(AGPasteboardLink *link, NSError **err))body
{
  @synchronized([self pasteboardLock]) {
    for (int attempt = 0; attempt < 2; attempt++) {
      AGPasteboardLink *link = [self pasteboardLinkWithError:error];
      if (!link) {
        return NO;
      }
      NSError *err = nil;
      @try {
        if (body(link, &err)) {
          return YES;
        }
      } @catch (NSException *e) {
        err = AGCapError(@"pasteboard raised %@: %@", e.name, e.reason);
        link.dead = YES;
      }
      if (!link.dead || attempt == 1) {
        AG_CAP_FAIL(error, err);
        return NO;
      }
    }
    return NO;
  }
}

static const NSTimeInterval AGPasteboardTimeout = 3;

- (NSString *)pasteboardStringWithError:(NSError **)error
{
  __block NSString *out = nil;
  BOOL ok = [self withPasteboard:error
                            body:^BOOL(AGPasteboardLink *link, NSError **err) {
                              if (![link pull:AGPasteboardTimeout error:err]) {
                                return NO;
                              }
                              out = [link.pasteboard stringForType:NSPasteboardTypeString] ?: @"";
                              return YES;
                            }];
  return ok ? out : nil;
}

// Sends the private pasteboard to the device, then pulls it back: once that pull is applied the
// device has the new content (the requests are ordered on one connection).
static BOOL AGPushAndConfirm(AGPasteboardLink *link, NSError **err)
{
  [link.iface push];
  return [link pull:AGPasteboardTimeout error:err];
}

- (BOOL)setPasteboardString:(NSString *)text error:(NSError **)error
{
  return [self withPasteboard:error
                         body:^BOOL(AGPasteboardLink *link, NSError **err) {
                           [link.pasteboard clearContents];
                           if (text.length > 0) {
                             [link.pasteboard setString:text forType:NSPasteboardTypeString];
                           }
                           return AGPushAndConfirm(link, err);
                         }];
}

- (NSArray *)pasteboardItemsWithError:(NSError **)error
{
  __block NSMutableArray *items = nil;
  BOOL ok = [self withPasteboard:error
                            body:^BOOL(AGPasteboardLink *link, NSError **err) {
                              if (![link pull:AGPasteboardTimeout error:err]) {
                                return NO;
                              }
                              items = [NSMutableArray array];
                              for (NSPasteboardItem *item in link.pasteboard.pasteboardItems) {
                                NSMutableDictionary *copy = [NSMutableDictionary dictionary];
                                for (NSPasteboardType type in item.types) {
                                  NSData *data = [item dataForType:type];
                                  if (data) {
                                    copy[type] = data;
                                  }
                                }
                                [items addObject:copy];
                              }
                              return YES;
                            }];
  return ok ? items : nil;
}

- (BOOL)setPasteboardItems:(NSArray *)items error:(NSError **)error
{
  return [self withPasteboard:error
                         body:^BOOL(AGPasteboardLink *link, NSError **err) {
                           [link.pasteboard clearContents];
                           NSMutableArray *objects = [NSMutableArray array];
                           for (NSDictionary *copy in items) {
                             NSPasteboardItem *item = [NSPasteboardItem new];
                             for (NSPasteboardType type in copy) {
                               [item setData:copy[type] forType:type];
                             }
                             [objects addObject:item];
                           }
                           if (objects.count > 0) {
                             [link.pasteboard writeObjects:objects];
                           }
                           return AGPushAndConfirm(link, err);
                         }];
}

@end
