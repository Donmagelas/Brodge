//go:build darwin && cgo

package codexdesktop

import (
	"context"
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
static AXError fixtureSettable(AXUIElementRef element, CFStringRef key, Boolean *out);
static void fixturePost(pid_t pid, CGEventRef event);
#define AXUIElementCopyAttributeValue fixtureAttribute
#define AXUIElementIsAttributeSettable fixtureSettable
#define CGEventPostToPid fixturePost
#include %q
#undef AXUIElementCopyAttributeValue
#undef AXUIElementIsAttributeSettable
#undef CGEventPostToPid
static AXError fixtureAttribute(AXUIElementRef element, CFStringRef key, CFTypeRef *out) {
    id value=[(NSDictionary *)element objectForKey:(id)key];
    if(!value) return kAXErrorAttributeUnsupported;
    *out=CFRetain((CFTypeRef)value); return kAXErrorSuccess;
}
static AXError fixtureSettable(AXUIElementRef element, CFStringRef key, Boolean *out) {
    *out=true; return kAXErrorSuccess;
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
        // Five references to a single field must remain five matches but have
        // one identity. The report includes only lengths, never user text.
        input[@"AXValue"]=@"private draft must never be logged";
        input[@"AXTitle"]=@"private title must never be logged";
        CGPoint point={42,80}; CGSize size={300,100};
        input[@"AXPosition"]=[(id)AXValueCreate(kAXValueCGPointType,&point) autorelease];
        input[@"AXSize"]=[(id)AXValueCreate(kAXValueCGSizeType,&size) autorelease];
        input[@"AXFocused"]=@YES;
        input[@"AXParent"]=@{@"AXRole":@"AXGroup",@"AXTitle":@"private parent title"};
        window[@"AXChildren"]=@[input,input,input,input,input];
        NSDictionary *report=composerInspection((AXUIElementRef)window);
        check([report[@"inputs"] intValue]==5 && [report[@"reported_unique_nodes"] intValue]==1,"duplicate references not distinguished");
        check([report[@"traversal_budget_ok"] boolValue] && [report[@"details_budget_ok"] boolValue],"inspection incorrectly exceeded budget");
        NSDictionary *match=report[@"matches"][0];
        check([match[@"node"] intValue]==1 && [match[@"frame"][@"x"] intValue]==42 && [match[@"focused"] boolValue],"input evidence missing");
        check([match[@"hidden"] isEqual:[NSNull null]],"unsupported visibility guessed");
        check([match[@"ancestors"][0][@"role"] isEqual:@"AXGroup"],"ancestor structure missing");
        NSData *json=[NSJSONSerialization dataWithJSONObject:report options:0 error:nil];
        NSString *encoded=[[[NSString alloc] initWithData:json encoding:NSUTF8StringEncoding] autorelease];
        check(json && ![encoded containsString:@"private"],"diagnostic leaked UI text");
        window[@"AXChildren"]=@[input,@{@"AXRole":@"AXTextArea",@"fixture_id":@2},@{@"AXRole":@"AXTextArea",@"fixture_id":@3},@{@"AXRole":@"AXTextArea",@"fixture_id":@4},@{@"AXRole":@"AXTextArea",@"fixture_id":@5}];
        report=composerInspection((AXUIElementRef)window);
        check([report[@"inputs"] intValue]==5 && [report[@"reported_unique_nodes"] intValue]==5,"distinct fields collapsed");
        check(posted==2,"inspection sent keyboard input");
        window[@"AXChildren"]=@[search];
        check(!composer((AXUIElementRef)window,NULL),"ordinary text field mistaken for composer");
        deadline=0;
        report=composerInspection((AXUIElementRef)window);
        check(![report[@"traversal_budget_ok"] boolValue],"timeout passed as complete evidence");
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

func TestComposerInspectionDoesNotRunDuringCreationOrAfterCancel(t *testing.T) {
	a := &Agent{}
	a.creationMu.Lock()
	if _, err := a.InspectConversationComposer(context.Background()); err == nil {
		t.Fatal("inspection allowed during creation")
	}
	a.creationMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.InspectConversationComposer(ctx); err != context.Canceled {
		t.Fatalf("cancelled inspection: %v", err)
	}
}
