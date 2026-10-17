// Opening the app while it runs does not start it again: macOS sends the running one a "reopen" event.
// Quitting it from outside — Activity Monitor, AppleScript, logging out — sends a "quit" event. The menu
// bar library passes on neither, so both are taken here and handed to Go.
#import <Cocoa/Cocoa.h>

extern void goReopen(void);
extern void goQuit(void);

@interface StowReopen : NSObject
- (void)reopen:(NSAppleEventDescriptor *)event withReplyEvent:(NSAppleEventDescriptor *)reply;
- (void)quit:(NSAppleEventDescriptor *)event withReplyEvent:(NSAppleEventDescriptor *)reply;
@end

@implementation StowReopen
- (void)reopen:(NSAppleEventDescriptor *)event withReplyEvent:(NSAppleEventDescriptor *)reply {
	goReopen();
}
- (void)quit:(NSAppleEventDescriptor *)event withReplyEvent:(NSAppleEventDescriptor *)reply {
	goQuit();
}
@end

static StowReopen *stowReopen;

void listenReopen(void) {
	dispatch_async(dispatch_get_main_queue(), ^{
		stowReopen = [StowReopen new];
		[[NSAppleEventManager sharedAppleEventManager] setEventHandler:stowReopen
			andSelector:@selector(reopen:withReplyEvent:) forEventClass:kCoreEventClass andEventID:kAEReopenApplication];
		[[NSAppleEventManager sharedAppleEventManager] setEventHandler:stowReopen
			andSelector:@selector(quit:withReplyEvent:) forEventClass:kCoreEventClass andEventID:kAEQuitApplication];
	});
}
