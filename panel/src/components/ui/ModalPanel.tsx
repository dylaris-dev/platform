// The implementation lives in the shared file-browser package, because the
// package's own modals (the file editor among them) cannot import from the
// panel. Panel code keeps importing it from here.
export { default } from '@dylaris/ui-filebrowser/src/ModalPanel';
