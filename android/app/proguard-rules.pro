# -keepattributes *Annotation*
# -keepclassmembers class * {
#     @org.webkit.net.* <methods>;
# }
-keep class mobile.** { *; }
-keep class go.** { *; }
