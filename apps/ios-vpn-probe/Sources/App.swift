import SwiftUI

// Minimal app whose only purpose is to find out whether a free Apple
// developer account can be granted the packet-tunnel entitlement that a
// system-wide bonding VPN would require.
@main
struct VPNProbeApp: App {
    var body: some Scene { WindowGroup { Text("probe") } }
}
