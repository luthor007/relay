import Foundation
import UIKit

/// A stable id for this phone, for the box's logs.
///
/// `identifierForVendor` is stable while any app from this vendor is installed
/// and changes when the last one is removed, which is the right lifetime: it
/// identifies an install, not a person, and it resets when the install does.
/// It is nil in rare states — before first unlock, most notably — so the value
/// is captured once and kept, rather than read at each use and occasionally
/// coming back different.
enum DeviceIdentity {

    private static let key = "device.id"

    static let current: String = {
        if let existing = UserDefaults.standard.string(forKey: key) {
            return existing
        }
        let id = UIDevice.current.identifierForVendor?.uuidString ?? UUID().uuidString
        UserDefaults.standard.set(id, forKey: key)
        return id
    }()
}
