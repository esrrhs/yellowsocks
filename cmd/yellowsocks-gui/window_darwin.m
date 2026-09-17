#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>
#include "window_darwin.h"

@interface YellowSocksWindowDelegate : NSObject <NSWindowDelegate>
@end

@implementation YellowSocksWindowDelegate
// Hide the window instead of destroying/quitting when user clicks the red close button
- (BOOL)windowShouldClose:(NSWindow *)sender {
    [sender orderOut:nil];
    return NO;
}
@end

static NSWindowController *g_windowController = nil;
static NSWindow *g_window = nil;
static WKWebView *g_webView = nil;
static YellowSocksWindowDelegate *g_delegate = nil;

void configureAppWindow(const char* title, int width, int height) {
    if (g_windowController != nil) {
        return;
    }

    NSApplication *app = [NSApplication sharedApplication];
    [app setActivationPolicy:NSApplicationActivationPolicyRegular];

    NSRect frame = NSMakeRect(0, 0, width, height);
    NSWindowStyleMask mask = NSWindowStyleMaskTitled |
                             NSWindowStyleMaskResizable |
                             NSWindowStyleMaskClosable |
                             NSWindowStyleMaskMiniaturizable;

    g_window = [[NSWindow alloc] initWithContentRect:frame
                                           styleMask:mask
                                             backing:NSBackingStoreBuffered
                                               defer:NO];
    [g_window setTitle:[NSString stringWithUTF8String:title]];
    [g_window center];

    g_delegate = [[YellowSocksWindowDelegate alloc] init];
    [g_window setDelegate:g_delegate];

    // Configure WKWebView
    WKWebViewConfiguration *config = [[WKWebViewConfiguration alloc] init];
    NSView *contentView = [g_window contentView];
    g_webView = [[WKWebView alloc] initWithFrame:[contentView bounds] configuration:config];
    [g_webView setTranslatesAutoresizingMaskIntoConstraints:NO];
    [contentView addSubview:g_webView];

    [contentView addConstraint:[NSLayoutConstraint constraintWithItem:g_webView
                                                            attribute:NSLayoutAttributeWidth
                                                            relatedBy:NSLayoutRelationEqual
                                                               toItem:contentView
                                                            attribute:NSLayoutAttributeWidth
                                                            multiplier:1
                                                              constant:0]];
    [contentView addConstraint:[NSLayoutConstraint constraintWithItem:g_webView
                                                            attribute:NSLayoutAttributeHeight
                                                            relatedBy:NSLayoutRelationEqual
                                                               toItem:contentView
                                                            attribute:NSLayoutAttributeHeight
                                                            multiplier:1
                                                              constant:0]];

    g_windowController = [[NSWindowController alloc] initWithWindow:g_window];

    [NSApp run];
}

void showAppWindow(const char* url) {
    if (url == NULL) return;
    NSString *urlString = [NSString stringWithUTF8String:url];

    dispatch_async(dispatch_get_main_queue(), ^{
        if (g_windowController == nil || g_window == nil || g_webView == nil) {
            return;
        }

        NSURL *nsURL = [NSURL URLWithString:urlString];
        if (nsURL != nil) {
            NSURLRequest *req = [NSURLRequest requestWithURL:nsURL
                                                 cachePolicy:NSURLRequestUseProtocolCachePolicy
                                             timeoutInterval:10];
            [g_webView loadRequest:req];
        }

        [g_windowController showWindow:g_window];
        [g_window makeKeyAndOrderFront:nil];
        [NSApp activateIgnoringOtherApps:YES];
    });
}
