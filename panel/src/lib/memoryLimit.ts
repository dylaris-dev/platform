// Wording for a node whose Docker cannot enforce a server's RAM limit: the
// host kernel has no memory cgroup, which is Raspberry Pi OS until it is
// switched on. A server there can take the whole machine. The node reports it
// in its heartbeat (noMemoryLimit); one sentence, shared by every view that
// shows a node.

export const NO_MEMORY_LIMIT_SHORT = 'RAM not limited';

export const NO_MEMORY_LIMIT_MESSAGE =
    'This machine cannot hold a server to its RAM, so one server can use all of it. ' +
    "On Raspberry Pi OS add 'cgroup_enable=memory' to the end of the line in " +
    '/boot/firmware/cmdline.txt and reboot.';
