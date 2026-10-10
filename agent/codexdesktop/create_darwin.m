//go:build darwin && cgo

#import <AppKit/AppKit.h>
#import <ApplicationServices/ApplicationServices.h>
#import <Carbon/Carbon.h>
#include <stdint.h>
#include <unistd.h>

// The adapter targets AX elements in the installed Desktop, never coordinates
// or an arbitrary foreground input. All error strings are fixed diagnostics.
typedef struct {
    AXUIElementRef app, window, input, send;
    pid_t pid;
    char *prompt;
    bool submitted;
} BrodgeDraft;

// Each cgo call gets a bounded AX traversal, including nested menu searches.
// A hung Desktop cannot hold the worker indefinitely during prepare or send.
static __thread NSTimeInterval deadline;
static bool withinDeadline(void) { return [NSProcessInfo processInfo].systemUptime < deadline; }

static id attr(AXUIElementRef el, CFStringRef key) {
    CFTypeRef value = NULL;
    if (!el || !withinDeadline() || AXUIElementCopyAttributeValue(el, key, &value) != kAXErrorSuccess) return nil;
    return [(id)value autorelease];
}
static NSString *stringAttr(AXUIElementRef el, CFStringRef key) {
    id v = attr(el, key);
    return [v isKindOfClass:[NSString class]] ? v : @"";
}
static bool enabled(AXUIElementRef el) { return [attr(el, kAXEnabledAttribute) boolValue]; }
static bool labelled(AXUIElementRef el, NSArray *labels) {
    for (NSString *key in @[@"AXTitle", @"AXDescription", @"AXPlaceholderValue"])
        if ([labels containsObject:stringAttr(el, (CFStringRef)key)]) return true;
    return false;
}
static void collect(AXUIElementRef root, NSString *role, NSArray *labels, NSMutableArray *out, int depth, int *budget) {
    if (!root || !withinDeadline() || depth > 60 || --*budget < 0) return;
    if ([stringAttr(root, kAXRoleAttribute) isEqualToString:role] && (!labels || labelled(root, labels))) [out addObject:(id)root];
    id children = attr(root, kAXChildrenAttribute);
    if ([children isKindOfClass:[NSArray class]]) for (id child in children) collect((AXUIElementRef)child, role, labels, out, depth+1, budget);
}
static AXUIElementRef unique(AXUIElementRef root, NSString *role, NSArray *labels) {
    NSMutableArray *found = [NSMutableArray array]; int budget = 12000;
    collect(root, role, labels, found, 0, &budget);
    if (!withinDeadline() || budget < 0 || found.count != 1) return NULL;
    return (AXUIElementRef)CFRetain((CFTypeRef)found[0]);
}
static AXUIElementRef menu(AXUIElementRef app, NSArray *labels) {
    return unique((AXUIElementRef)attr(app, kAXMenuBarAttribute), @"AXMenuItem", labels);
}
static bool press(AXUIElementRef el) { return el && withinDeadline() && enabled(el) && AXUIElementPerformAction(el, kAXPressAction) == kAXErrorSuccess; }
static bool sameWindow(BrodgeDraft *d) {
    id window = attr(d->app, kAXFocusedWindowAttribute);
    return window && CFEqual((CFTypeRef)window, d->window) && [NSWorkspace sharedWorkspace].frontmostApplication.processIdentifier == d->pid;
}
static bool shortcut(BrodgeDraft *d, CGKeyCode key) {
    if (!withinDeadline() || [NSWorkspace sharedWorkspace].frontmostApplication.processIdentifier != d->pid) return false;
    CGEventRef down=CGEventCreateKeyboardEvent(NULL,key,true), up=CGEventCreateKeyboardEvent(NULL,key,false);
    if (!down || !up) { if(down) CFRelease(down); if(up) CFRelease(up); return false; }
    CGEventSetFlags(down,kCGEventFlagMaskCommand|kCGEventFlagMaskAlternate);
    CGEventSetFlags(up,kCGEventFlagMaskCommand|kCGEventFlagMaskAlternate);
    CGEventPostToPid(d->pid,down); CGEventPostToPid(d->pid,up);
    CFRelease(down); CFRelease(up); return true;
}
static NSArray *saveClipboard(NSPasteboard *pb) {
    NSMutableArray *saved = [NSMutableArray array];
    for (NSPasteboardItem *item in pb.pasteboardItems) {
        NSPasteboardItem *copy = [[[NSPasteboardItem alloc] init] autorelease];
        for (NSPasteboardType type in item.types) { NSData *data = [item dataForType:type]; if (data) [copy setData:data forType:type]; }
        [saved addObject:copy];
    }
    return saved;
}
static void restoreClipboard(NSPasteboard *pb, NSArray *saved, NSInteger owned) {
    if (pb.changeCount != owned) return; // A user copy wins over our restoration.
    [pb clearContents]; if (saved.count) [pb writeObjects:saved];
}
static NSString *copyLink(BrodgeDraft *d) {
    if (!sameWindow(d)) return nil;
    NSPasteboard *pb = [NSPasteboard generalPasteboard]; NSArray *saved = saveClipboard(pb);
    NSInteger before = pb.changeCount;
    bool ok = shortcut(d,kVK_ANSI_L);
    for (int i=0; ok && withinDeadline() && i<20 && pb.changeCount==before; i++) usleep(50000);
    NSInteger owned = pb.changeCount;
    // A new empty chat has no link; Desktop deliberately leaves the clipboard alone.
    NSString *link = ok ? (owned != before ? [[[pb stringForType:NSPasteboardTypeString] copy] autorelease] : @"") : nil;
    if (owned != before) restoreClipboard(pb, saved, owned);
    return link;
}
static bool hasFileLabel(AXUIElementRef root, NSString *name, int depth, int *budget) {
    if (!root || !withinDeadline() || depth>60 || --*budget<0) return false;
    for (NSString *key in @[@"AXTitle", @"AXDescription", @"AXValue"])
        if ([stringAttr(root, (CFStringRef)key) containsString:name]) return true;
    id children=attr(root,kAXChildrenAttribute);
    if ([children isKindOfClass:[NSArray class]]) for(id c in children) if(hasFileLabel((AXUIElementRef)c,name,depth+1,budget)) return true;
    return false;
}
static bool paste(BrodgeDraft *d, NSString *text, NSArray *paths) {
    if (!sameWindow(d) || AXUIElementSetAttributeValue(d->input,kAXFocusedAttribute,kCFBooleanTrue)!=kAXErrorSuccess) return false;
    id focused = attr(d->app,kAXFocusedUIElementAttribute);
    if (!focused || !CFEqual((CFTypeRef)focused,d->input)) return false;
    AXUIElementRef item=menu(d->app,@[@"Paste",@"粘贴",@"貼上"]);
    if (!item || !enabled(item)) { if(item) CFRelease(item); return false; }
    NSPasteboard *pb=[NSPasteboard generalPasteboard]; NSArray *saved=saveClipboard(pb);
    [pb clearContents];
    if (text) [pb setString:text forType:NSPasteboardTypeString];
    else { NSMutableArray *urls=[NSMutableArray array]; for(NSString *path in paths) [urls addObject:[NSURL fileURLWithPath:path]]; [pb writeObjects:urls]; }
    NSInteger owned=pb.changeCount;
    bool ok=sameWindow(d) && press(item); CFRelease(item);
    bool observed=false;
    for(int i=0;ok && withinDeadline() && i<40;i++) {
        usleep(100000);
        if(!sameWindow(d)) break;
        if(text) observed=[stringAttr(d->input,kAXValueAttribute) isEqualToString:text];
        else { observed=true; for(NSString *path in paths) { int budget=12000; if(!hasFileLabel(d->window,path.lastPathComponent,0,&budget)) observed=false; } }
        if(observed) break;
    }
    restoreClipboard(pb,saved,owned);
    return observed;
}

void brodgeCloseDraft(uintptr_t handle) {
    BrodgeDraft *d=(BrodgeDraft *)handle; if(!d) return;
    if(d->app) CFRelease(d->app); if(d->window) CFRelease(d->window);
    if(d->input) CFRelease(d->input); if(d->send) CFRelease(d->send);
    free(d->prompt); free(d);
}
uintptr_t brodgePrepareDraft(const char *prompt, const char *files, char **error) {
    @autoreleasepool {
        deadline=[NSProcessInfo processInfo].systemUptime+20;
        NSDictionary *options=@{(id)kAXTrustedCheckOptionPrompt:@YES};
        if(!AXIsProcessTrustedWithOptions((CFDictionaryRef)options)) { *error=strdup("请在 macOS 系统设置 → 隐私与安全性 → 辅助功能中允许 Brodge，然后在 Web 重试创建"); return 0; }
        NSArray *apps=[NSRunningApplication runningApplicationsWithBundleIdentifier:@"com.openai.codex"];
        if(apps.count!=1) { *error=strdup("请先打开 Codex Desktop，再重试创建"); return 0; }
        NSDictionary *version=[[NSBundle bundleWithURL:[apps[0] bundleURL]] infoDictionary];
        if(![version[@"CFBundleShortVersionString"] isEqualToString:@"26.928.21956"] || ![version[@"CFBundleVersion"] isEqualToString:@"12404"]) {
            *error=strdup("此 Desktop 版本的界面创建尚未适配，请先绑定已有会话"); return 0;
        }
        BrodgeDraft *d=calloc(1,sizeof(BrodgeDraft)); d->pid=[apps[0] processIdentifier]; d->prompt=strdup(prompt);
        d->app=AXUIElementCreateApplication(d->pid); AXUIElementSetMessagingTimeout(d->app,2);
        [apps[0] activateWithOptions:0];
        for(int i=0;withinDeadline() && i<20 && [NSWorkspace sharedWorkspace].frontmostApplication.processIdentifier!=d->pid;i++) usleep(100000);
        if(!shortcut(d,kVK_ANSI_O)) { *error=strdup("desktop-new-standalone-shortcut-unavailable"); goto fail; }
        for(int i=0;withinDeadline() && i<40;i++) {
            usleep(100000);
            id window=attr(d->app,kAXFocusedWindowAttribute); if(!window) continue;
            AXUIElementRef input=unique((AXUIElementRef)window,@"AXTextArea",@[@"Do anything",@"随心输入",@"想做什麼都可以"]);
            if(!input) continue;
            if(![stringAttr(input,kAXValueAttribute) isEqualToString:@""]) { CFRelease(input); break; }
            d->input=input; d->window=(AXUIElementRef)CFRetain((CFTypeRef)window); break;
        }
        if(!d->input || !sameWindow(d)) { *error=strdup("desktop-empty-composer-not-confirmed"); goto fail; }
        { NSString *before=copyLink(d); if(!before || [before hasPrefix:@"codex://threads/"]) { *error=strdup("desktop-new-page-not-confirmed"); goto fail; } }
        { NSData *data=[[NSString stringWithUTF8String:files] dataUsingEncoding:NSUTF8StringEncoding];
          NSArray *paths=[NSJSONSerialization JSONObjectWithData:data options:0 error:nil];
          if(paths.count && !paste(d,nil,paths)) { *error=strdup("desktop-attachment-draft-not-confirmed"); goto fail; } }
        if(!paste(d,[NSString stringWithUTF8String:prompt],nil)) { *error=strdup("desktop-text-draft-not-confirmed"); goto fail; }
        d->send=unique(d->window,@"AXButton",@[@"Send",@"发送",@"傳送"]);
        if(!d->send || !enabled(d->send)) { *error=strdup("desktop-send-control-unavailable"); goto fail; }
        return (uintptr_t)d;
    fail:
        brodgeCloseDraft((uintptr_t)d); return 0;
    }
}
char *brodgeSubmitDraft(uintptr_t handle, char **error) {
    @autoreleasepool {
        deadline=[NSProcessInfo processInfo].systemUptime+15;
        BrodgeDraft *d=(BrodgeDraft *)handle;
        if(!d || d->submitted || !sameWindow(d) || ![stringAttr(d->input,kAXValueAttribute) isEqualToString:[NSString stringWithUTF8String:d->prompt]]) { *error=strdup("desktop-draft-changed-before-send"); return NULL; }
        AXUIElementRef input=unique(d->window,@"AXTextArea",@[@"Do anything",@"随心输入",@"想做什麼都可以"]);
        bool stillInput=input && CFEqual(input,d->input); if(input) CFRelease(input);
        NSString *before=copyLink(d);
        if(!stillInput || !before || [before hasPrefix:@"codex://threads/"]) { *error=strdup("desktop-new-page-changed-before-send"); return NULL; }
        d->submitted=true; // At most one AX press, even if its return value is lost.
        if(!press(d->send)) { *error=strdup("desktop-send-outcome-unknown"); return NULL; }
        for(int i=0;withinDeadline() && i<40;i++) {
            usleep(200000);
            NSString *link=copyLink(d);
            if([link hasPrefix:@"codex://threads/"]) return strdup(link.UTF8String);
            if(!sameWindow(d)) break;
        }
        *error=strdup("desktop-created-link-not-confirmed"); return NULL;
    }
}
