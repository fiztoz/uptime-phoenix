// Phoenix Database monitor — least-privilege MongoDB user
// Edit password and roles, then run as admin:
//   mongosh "mongodb://admin:…@HOST:27017/admin" create-user-mongodb.js
//
// Grants: read on the target database (enough for connect + ping).
// Does NOT grant readWrite or clusterAdmin.

const user = "phoenix_monitor";
const password = "CHANGE_ME_STRONG_PASSWORD"; // === EDIT ===
const targetDb = "appdb"; // === EDIT ===

const admin = db.getSiblingDB("admin");

try {
  admin.createUser({
    user: user,
    pwd: password,
    roles: [
      { role: "read", db: targetDb },
      // clusterMonitor is optional; not required for Ping
    ],
  });
  print("Created user " + user);
} catch (e) {
  if (String(e).includes("already exists") || e.code === 51003) {
    admin.updateUser(user, {
      pwd: password,
      roles: [{ role: "read", db: targetDb }],
    });
    print("Updated user " + user);
  } else {
    throw e;
  }
}

// Verify:
// mongosh "mongodb://phoenix_monitor:…@HOST:27017/appdb?authSource=admin" --eval 'db.runCommand({ ping: 1 })'

// Phoenix connection string example:
// mongodb://phoenix_monitor:CHANGE_ME_STRONG_PASSWORD@HOST:27017/appdb?authSource=admin
