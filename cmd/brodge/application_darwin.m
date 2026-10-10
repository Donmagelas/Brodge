//go:build darwin && cgo

#import <AppKit/AppKit.h>
extern void brodgeAppReopen(void);

@interface BrodgeAppDelegate : NSObject <NSApplicationDelegate>
@end
@implementation BrodgeAppDelegate
- (BOOL)applicationShouldHandleReopen:(NSApplication *)app hasVisibleWindows:(BOOL)visible {
    brodgeAppReopen();
    return NO;
}
@end

void brodgeAppRun(void) {
    @autoreleasepool {
        NSApplication *app = [NSApplication sharedApplication];
        BrodgeAppDelegate *delegate = [BrodgeAppDelegate new];
        [app setDelegate:delegate];
        [app setActivationPolicy:NSApplicationActivationPolicyAccessory];
        [app run];
        [app setDelegate:nil];
    }
}

void brodgeAppStop(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
        [NSApp stop:nil];
        [NSApp postEvent:[NSEvent otherEventWithType:NSEventTypeApplicationDefined
            location:NSZeroPoint modifierFlags:0 timestamp:0 windowNumber:0
            context:nil subtype:0 data1:0 data2:0] atStart:NO];
    });
}
