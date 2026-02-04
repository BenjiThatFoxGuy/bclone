#!/usr/bin/env python3
import subprocess,os,sys
root=os.getcwd()
files=subprocess.check_output(['git','ls-files','*.go'],cwd=root).decode().splitlines()
changed=[]
for f in files:
    p=os.path.join(root,f)
    with open(p,'rb') as fh:
        data=fh.read()
    text=None
    try:
        text=data.decode('utf-8')
    except Exception:
        # try utf-16 variants
        for enc in ('utf-16','utf-16-le','utf-16-be'):
            try:
                text=data.decode(enc)
                break
            except Exception:
                continue
    if text is None:
        print('Skipping (cannot decode):',f)
        continue
    # strip BOM
    if text and text[0]=='\ufeff':
        text=text[1:]
    # normalize newlines to \n
    text=text.replace('\r\n','\n').replace('\r','\n')
    # write back as utf-8
    with open(p,'wb') as fh:
        fh.write(text.encode('utf-8'))
    changed.append(f)
print('Processed files:',len(files),'Rewritten:',len(changed))
for c in changed[:200]:
    print(c)
