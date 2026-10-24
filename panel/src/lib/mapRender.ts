// The console command that renders a whole web map once.
//   squaremap / Pl3xMap: `map fullrender <world>` (squaremap wiki, Commands)
//   BlueMap: `bluemap update` renders every tile it has not rendered yet
// The world name is server.properties' level-name; anything that is not a plain
// name falls back to "world" rather than reaching the console.
export function mapRenderCommand(kind: 'squaremap' | 'bluemap', world: string): string {
    if (kind === 'bluemap') return 'bluemap update';
    const w = /^[A-Za-z0-9_.-]{1,64}$/.test(world) ? world : 'world';
    return `map fullrender ${w}`;
}
