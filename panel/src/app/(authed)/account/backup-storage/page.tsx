import { redirect } from 'next/navigation';

// Backup storage became Storage: the same targets, plus where templates go. The
// old address stays as a redirect because it was a real URL somebody could
// bookmark or find in an error message.
export default function BackupStorageRedirect() {
    redirect('/account/storage');
}
