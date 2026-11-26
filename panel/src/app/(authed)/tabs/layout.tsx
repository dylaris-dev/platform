// The authed shell renders pages bare inside a flex row that does not scroll,
// so this route supplies the scrolling page frame (padding + 1400px centring).
export default function Layout({ children }: { children: React.ReactNode }) {
    return <main className="flex-1 min-h-0 overflow-y-auto app-page">{children}</main>;
}
