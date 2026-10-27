import PackWorkspace from '@/components/modpacks/PackWorkspace';

export default function Layout({ children }: { children: React.ReactNode }) {
    return <PackWorkspace>{children}</PackWorkspace>;
}
