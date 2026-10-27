import { describe,expect,it } from 'vitest';
import { fileActionDestination } from '../../../packages/ui-filebrowser/src/utils';
describe('file browser operation destinations',()=>{
    it('creates files and folders inside the currently open folder',()=>{
        expect(fileActionDestination('kubejs/server_scripts','','recipes.js',true)).toBe('kubejs/server_scripts/recipes.js');
        expect(fileActionDestination('config/','','nested',true)).toBe('config/nested');
        expect(fileActionDestination('','','root.json',true)).toBe('root.json');
    });
    it('keeps copy/rename in the source parent, including global search results',()=>{
        expect(fileActionDestination('config','kubejs/server_scripts/old.js','new.js',false)).toBe('kubejs/server_scripts/new.js');
        expect(fileActionDestination('config','root.txt','new.txt',false)).toBe('new.txt');
    });
});
