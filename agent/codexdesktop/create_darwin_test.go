//go:build darwin && cgo

package codexdesktop

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestNativeComposerSelection(t *testing.T) {
	// Compile the actual selector with a dictionary-backed AX boundary. This
	// exercises native lookup without inspecting or operating any real app.
	native, err := filepath.Abs("create_darwin.m")
	if err != nil {
		t.Fatal(err)
	}
	source := fmt.Sprintf(`#import <AppKit/AppKit.h>
#import <ApplicationServices/ApplicationServices.h>
static AXError fixtureAttribute(AXUIElementRef element, CFStringRef key, CFTypeRef *out);
static void fixturePost(pid_t pid, CGEventRef event);
#define AXUIElementCopyAttributeValue fixtureAttribute
#define CGEventPostToPid fixturePost
#include %q
#undef AXUIElementCopyAttributeValue
#undef CGEventPostToPid
static AXError fixtureAttribute(AXUIElementRef element, CFStringRef key, CFTypeRef *out) {
    id value=[(NSDictionary *)element objectForKey:(id)key];
    if(!value) return kAXErrorAttributeUnsupported;
    *out=CFRetain((CFTypeRef)value); return kAXErrorSuccess;
}
static void check(bool ok, const char *message) {
    if(!ok) { fprintf(stderr,"%%s\n",message); exit(1); }
}
static int posted=0;
static void fixturePost(pid_t pid, CGEventRef event) {
    check(pid==42,"key delivered to wrong process");
    check(CGEventGetType(event)==(posted==0?kCGEventKeyDown:kCGEventKeyUp),"key event order changed");
    check(CGEventGetIntegerValueField(event,kCGKeyboardEventKeycode)==kVK_Return,"wrong submit key");
    check(CGEventGetFlags(event)==kCGEventFlagMaskCommand,"submit modifiers changed");
    posted++;
}
int main(void) {
    @autoreleasepool {
        deadline=[NSProcessInfo processInfo].systemUptime+30;
        check(keyPair(42,kVK_Return,kCGEventFlagMaskCommand),"submit key construction failed");
        check(posted==2,"submission dispatched more than one key pair");
        NSMutableDictionary *input=[@{@"AXRole":@"AXTextArea",@"AXValue":@""} mutableCopy];
        NSDictionary *search=@{@"AXRole":@"AXTextField",@"AXValue":@""};
        NSDictionary *send=@{@"AXRole":@"AXButton",@"AXTitle":@"发送",@"AXEnabled":@YES};
        NSMutableDictionary *window=[@{@"AXRole":@"AXWindow",@"AXChildren":@[search,input,send]} mutableCopy];
        NSString *diagnostic=nil;
        AXUIElementRef found=composer((AXUIElementRef)window,&diagnostic);
        check(found && CFEqual(found,(CFTypeRef)input),"unnamed multiline composer was not found");
        check(textEquals(found,@""),"empty composer rejected");
        check(emptyComposer(found),"empty composer rejected");
        input[@"AXValue"]=@"随心输入\n";
        check(emptyComposer(found),"decorated empty composer rejected");
        input[@"AXValue"]=@"随心输入user draft\n";
        check(!textEquals(found,@"user draft"),"text appended to an existing draft was accepted");
        input[@"AXValue"]=@"user draft";
        check(!emptyComposer(found),"nonempty draft accepted");
        check(!textEquals(found,@""),"nonempty draft treated as empty");
        check(textEquals(found,@"user draft"),"exact draft verification failed");
        input[@"AXValue"]=@"user draft\n";
        check(textEquals(found,@"user draft"),"AX paragraph separator rejected");
        input[@"AXValue"]=@"user draft\n\n";
        check(!textEquals(found,@"user draft"),"additional draft content ignored");
        [input removeObjectForKey:@"AXValue"];
        check(!textEquals(found,@""),"missing value treated as empty");
        input[@"AXValue"]=@0;
        check(!textEquals(found,@""),"nontext value treated as empty");
        CFRelease(found);
        window[@"AXChildren"]=@[input,@{@"AXRole":@"AXTextArea",@"AXValue":@""}];
        check(!composer((AXUIElementRef)window,&diagnostic),"multiple composers accepted");
        check([diagnostic containsString:@"inputs=2"],"ambiguous input diagnostic missing");
        window[@"AXChildren"]=@[search];
        check(!composer((AXUIElementRef)window,NULL),"ordinary text field mistaken for composer");
        [window release]; [input release];
    }
    return 0;
}
`, native)
	dir := t.TempDir()
	fixture := filepath.Join(dir, "composer.m")
	if err := os.WriteFile(fixture, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "composer-test")
	cmd := exec.Command("clang", "-framework", "AppKit", "-framework", "ApplicationServices", fixture, "-o", binary)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile native fixture: %v\n%s", err, output)
	}
	if output, err := exec.Command(binary).CombinedOutput(); err != nil {
		t.Fatalf("native selector: %v\n%s", err, output)
	}
}
