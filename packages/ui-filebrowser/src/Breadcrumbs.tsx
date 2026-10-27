import React from 'react';

export interface BreadcrumbsProps {
  rootLabel?: string;
  currentPath: string;
  onNavigate: (path: string) => void;
}

// Path breadcrumb trail. The implicit root crumb is "servers" (empty path);
// every segment before the last is a button that navigates to that path.
const Breadcrumbs: React.FC<BreadcrumbsProps> = ({ currentPath, onNavigate, rootLabel = 'servers' }) => {
  const pathSegments = currentPath.split('/').filter((p: string) => p);
  const breadcrumbs = [{ name: rootLabel, path: '' }];
  let cumulativePath = '';
  for (const segment of pathSegments) {
    cumulativePath = cumulativePath ? `${cumulativePath}/${segment}` : segment;
    breadcrumbs.push({ name: segment, path: cumulativePath });
  }
  return (
    <div className="flex items-center text-xl text-(--base-09) flex-wrap">
      {breadcrumbs.map((crumb, index) => (
        <React.Fragment key={crumb.path}>
          {index > 0 && <span className="mx-2 text-(--base-07)">/</span>}
          {index < breadcrumbs.length - 1 ? (
            <button onClick={() => onNavigate(crumb.path)} className="hover:text-(--primary-light) transition-colors">{crumb.name}</button>
          ) : (
            <span className="text-(--base-07)">{crumb.name}</span>
          )}
        </React.Fragment>
      ))}
    </div>
  );
};

export default Breadcrumbs;
