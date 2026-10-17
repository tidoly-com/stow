// The app window (stow issue #18): a WebKit view of the server's pages, in a process of its own.
#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>

// macOS hands ⌘C/⌘V/⌘X/⌘A/⌘Z to the Edit menu's items; webview makes no menu, so without one the
// keyboard cannot paste into the pages (tidoly's installer found this, 2026-07-26). ⌘Q and ⌘W close
// the window; the server goes on.
void windowMenu(void) {
	@autoreleasepool {
		NSApplication *app = [NSApplication sharedApplication];
		// The app has no Dock icon while it sits in the menu bar; a window does need one, or it cannot be
		// found with ⌘Tab nor quit with ⌘Q.
		[app setActivationPolicy:NSApplicationActivationPolicyRegular];
		NSMenu *bar = [[NSMenu alloc] init];
		NSMenuItem *appItem = [[NSMenuItem alloc] init];
		[bar addItem:appItem];
		NSMenu *appMenu = [[NSMenu alloc] init];
		[appMenu addItemWithTitle:@"창 닫기" action:@selector(terminate:) keyEquivalent:@"q"];
		[appMenu addItemWithTitle:@"창 닫기" action:@selector(terminate:) keyEquivalent:@"w"];
		[appItem setSubmenu:appMenu];
		NSMenuItem *editItem = [[NSMenuItem alloc] init];
		[bar addItem:editItem];
		NSMenu *edit = [[NSMenu alloc] initWithTitle:@"편집"];
		[edit addItemWithTitle:@"실행 취소" action:@selector(undo:) keyEquivalent:@"z"];
		[edit addItemWithTitle:@"다시 실행" action:@selector(redo:) keyEquivalent:@"Z"];
		[edit addItem:[NSMenuItem separatorItem]];
		[edit addItemWithTitle:@"잘라내기" action:@selector(cut:) keyEquivalent:@"x"];
		[edit addItemWithTitle:@"복사" action:@selector(copy:) keyEquivalent:@"c"];
		[edit addItemWithTitle:@"붙여넣기" action:@selector(paste:) keyEquivalent:@"v"];
		[edit addItemWithTitle:@"모두 선택" action:@selector(selectAll:) keyEquivalent:@"a"];
		[editItem setSubmenu:edit];
		[app setMainMenu:bar];
		[app activateIgnoringOtherApps:YES];
	}
}

// raiseApp brings the window process to the front — from the menu bar's process, which gives way to it.
void raiseApp(int pid) {
	dispatch_async(dispatch_get_main_queue(), ^{
		NSRunningApplication *window = [NSRunningApplication runningApplicationWithProcessIdentifier:pid];
		if (window == nil) {
			return;
		}
		[window unhide];
		if (@available(macOS 14, *)) {
			[NSApp yieldActivationToApplication:window];
			[window activateWithOptions:NSApplicationActivateAllWindows];
		} else {
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"
			[window activateWithOptions:NSApplicationActivateAllWindows | NSApplicationActivateIgnoringOtherApps];
#pragma clang diagnostic pop
		}
	});
}

// webview makes a delegate for <input type=file> but lets go of it at once, and WebKit holds its UI
// delegate weakly: the delegate is gone, and the upload button opened nothing. This one is kept.
@interface StowFilePanel : NSObject <WKUIDelegate>
@end

@implementation StowFilePanel
- (void)webView:(WKWebView *)webView runOpenPanelWithParameters:(WKOpenPanelParameters *)parameters
    initiatedByFrame:(WKFrameInfo *)frame completionHandler:(void (^)(NSArray<NSURL *> *))completionHandler {
	NSOpenPanel *panel = [NSOpenPanel openPanel];
	panel.canChooseFiles = YES;
	panel.canChooseDirectories = parameters.allowsDirectories;
	panel.allowsMultipleSelection = parameters.allowsMultipleSelection;
	[panel beginSheetModalForWindow:webView.window completionHandler:^(NSModalResponse result) {
		completionHandler(result == NSModalResponseOK ? panel.URLs : nil);
	}];
}
@end

static StowFilePanel *filePanel;

static WKWebView *findWebView(NSView *view) {
	if ([view isKindOfClass:[WKWebView class]]) {
		return (WKWebView *)view;
	}
	for (NSView *sub in view.subviews) {
		WKWebView *found = findWebView(sub);
		if (found != nil) {
			return found;
		}
	}
	return nil;
}

// fileDialogs gives the window's web view a file panel that stays.
void fileDialogs(void *window) {
	NSWindow *w = (__bridge NSWindow *)window;
	WKWebView *view = findWebView(w.contentView);
	if (view == nil) {
		return;
	}
	filePanel = [StowFilePanel new];
	view.UIDelegate = filePanel;
}
