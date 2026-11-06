// Declarations of the private CoreSimulator / SimulatorKit / AccessibilityPlatformTranslation
// selectors agemu uses. They are declared as protocols and messaged through `id`, because the
// frameworks are loaded at runtime and never linked.
//
// Selector names and signatures follow the class dumps published by facebook/idb
// (FBSimulatorControl, MIT license, PrivateHeaders/), checked against Xcode 26.6.
#import <AppKit/AppKit.h>
#import <Foundation/Foundation.h>

NS_ASSUME_NONNULL_BEGIN

@protocol AGSimServiceContext <NSObject>
+ (nullable id)sharedServiceContextForDeveloperDir:(NSString *)dir error:(NSError **)error;
- (nullable id)defaultDeviceSetWithError:(NSError **)error;
@end

@protocol AGSimDeviceSet <NSObject>
- (NSArray *)devices;
@end

@protocol AGSimDeviceType <NSObject>
- (NSString *)identifier;
- (NSString *)name;
- (CGSize)mainScreenSize;
- (float)mainScreenScale;
@end

@protocol AGSimRuntime <NSObject>
- (NSString *)identifier;
- (NSString *)versionString;
- (NSString *)root;
@end

@protocol AGSimDevice <NSObject>
- (NSUUID *)UDID;
- (NSString *)name;
- (unsigned long long)state;
- (NSString *)stateString;
- (id)deviceType;
- (id)runtime;
- (NSString *)dataPath;
- (nullable NSDate *)lastBootedAt;
- (nullable id)bootStatus;
- (nullable id)io;
- (BOOL)bootWithOptions:(NSDictionary *)options error:(NSError **)error;
- (BOOL)shutdownWithError:(NSError **)error;
- (int)launchApplicationWithID:(NSString *)bundleID options:(NSDictionary *)options error:(NSError **)error;
- (BOOL)terminateApplicationWithID:(NSString *)bundleID error:(NSError **)error;
- (BOOL)installApplication:(NSURL *)url withOptions:(NSDictionary *)options error:(NSError **)error;
- (BOOL)uninstallApplication:(NSString *)bundleID withOptions:(NSDictionary *)options error:(NSError **)error;
- (nullable NSDictionary *)installedAppsWithError:(NSError **)error;
- (int)spawnWithPath:(NSString *)path
              options:(NSDictionary *)options
     terminationQueue:(dispatch_queue_t)queue
   terminationHandler:(void (^)(int status))handler
                error:(NSError **)error;
- (BOOL)postDarwinNotification:(NSString *)name error:(NSError **)error;
- (void)sendAccessibilityRequestAsync:(id)request
                      completionQueue:(dispatch_queue_t)queue
                    completionHandler:(void (^)(id response))handler;
@end

// SimDeviceBootInfo: what `simctl bootstatus` prints. status: 0 booting, 1 waiting on
// BackBoard, 2 waiting on data migration, 4 waiting on the system app (SpringBoard),
// 0xFFFFFFFF finished (terminal).
@protocol AGSimDeviceBootInfo <NSObject>
- (unsigned int)status;
- (BOOL)isTerminalStatus;
- (double)bootElapsedTime;
@end

// SimDeviceIOClient
@protocol AGSimDeviceIO <NSObject>
- (NSArray *)ioPorts;
@end

@protocol AGSimDeviceIOPort <NSObject>
- (id)descriptor;
@end

@protocol AGSimDisplayDescriptorState <NSObject>
- (unsigned short)displayClass;
@end

// The display descriptor (a ROCK remote proxy).
@protocol AGSimDisplay <NSObject>
- (id)state;
- (nullable id)framebufferSurface;
- (nullable id)ioSurface;
- (void)registerCallbackWithUUID:(NSUUID *)uuid ioSurfaceChangeCallback:(void (^)(id _Nullable))callback;
- (void)registerCallbackWithUUID:(NSUUID *)uuid ioSurfacesChangeCallback:(void (^)(id _Nullable))callback;
- (void)registerScreenCallbacksWithUUID:(NSUUID *)uuid
                          callbackQueue:(dispatch_queue_t)queue
                          frameCallback:(void (^)(void))frameCallback
                surfacesChangedCallback:(void (^)(id _Nullable, id _Nullable))surfacesChangedCallback
              propertiesChangedCallback:(void (^)(id))propertiesChangedCallback;
@end

// SimulatorKit.SimDeviceLegacyHIDClient
@protocol AGLegacyHIDClient <NSObject>
- (nullable instancetype)initWithDevice:(id)device error:(NSError **)error;
- (void)sendWithMessage:(void *)message
           freeWhenDone:(BOOL)freeWhenDone
        completionQueue:(dispatch_queue_t)queue
             completion:(void (^)(NSError *_Nullable error))completion;
@end

// AccessibilityPlatformTranslation
typedef id _Nonnull (^AGTranslationCallback)(id request);

@protocol AGAXPTranslationTokenDelegateHelper <NSObject>
- (AGTranslationCallback)accessibilityTranslationDelegateBridgeCallbackWithToken:(NSString *)token;
- (CGRect)accessibilityTranslationConvertPlatformFrameToSystem:(CGRect)rect withToken:(NSString *)token;
- (nullable id)accessibilityTranslationRootParentWithToken:(NSString *)token;
@end

@protocol AGAXPTranslator <NSObject>
+ (id)sharedInstance;
- (void)setBridgeTokenDelegate:(id)delegate;
- (void)setSupportsDelegateTokens:(BOOL)supports;
- (nullable id)frontmostApplicationWithDisplayId:(unsigned int)displayId bridgeDelegateToken:(NSString *)token;
- (nullable id)objectAtPoint:(CGPoint)point displayId:(unsigned int)displayId bridgeDelegateToken:(NSString *)token;
- (nullable id)macPlatformElementFromTranslation:(id)translation;
@end

@protocol AGAXPTranslationObject <NSObject>
- (void)setBridgeDelegateToken:(NSString *)token;
- (int)pid;
- (unsigned long long)objectID;
@end

@protocol AGAXPTranslatorResponse <NSObject>
+ (id)emptyResponse;
- (unsigned long long)error;
- (nullable id)resultData;
@end

@protocol AGAXPTranslatorRequest <NSObject>
+ (id)requestWithTranslation:(nullable id)translation;
- (void)setRequestType:(unsigned long long)type;
- (void)setAttributeType:(unsigned long long)type;
- (void)setParameters:(nullable NSDictionary *)parameters;
- (void)setClientType:(unsigned long long)type;
@end

// AXPMacPlatformElement is an NSAccessibilityElement subclass; its NSAccessibility
// properties are fetched lazily, one translator request each.
@protocol AGAXPMacPlatformElement <NSObject>
- (id)translation;
/// Mac attribute names (AXRole, AXFrame, ...) -> converted values, fetched in one translator
/// request (type 5, multiple attributes).
- (nullable NSDictionary *)accessibilityMultipleAttributes:(NSArray<NSString *> *)attributes;
@end

NS_ASSUME_NONNULL_END
