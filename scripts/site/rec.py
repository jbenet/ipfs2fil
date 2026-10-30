"""Record a scripted terminal session to asciicast v2, typing like a person.

    HOME=/home/demo COLS=100 ROWS=32 python3 rec.py scenes/up.scene up.cast

Scene file: python list of steps: ('type', text) types+Enter, ('wait', regex[, timeout]),
('sleep', s), ('key', text) sends raw text without typing delay."""
import os, pty, sys, time, json, select, re, fcntl, termios, struct
cols, rows = int(os.environ.get('COLS', 96)), int(os.environ.get('ROWS', 26))
scene_file, out = sys.argv[1], sys.argv[2]
src = open(scene_file).read().split("\n", 1); PR = eval(src[0].split("=",1)[1]); steps = eval(src[1])
home = os.environ['HOME']
env = dict(os.environ, TERM='xterm-256color',
           PS1='\\[\\e[1;34m\\]~\\[\\e[0m\\] \\[\\e[1;32m\\]$\\[\\e[0m\\] ', PROMPT_COMMAND='')
pid, fd = pty.fork()
if pid == 0:
    os.chdir(home)
    os.execvpe('bash', ['bash', '--norc', '--noprofile', '-i'], env)
fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack('HHHH', rows, cols, 0, 0))
t0 = time.time(); events = []; buf = ''
def pump(t=0.05):
    global buf
    r, _, _ = select.select([fd], [], [], t)
    if r:
        try: d = os.read(fd, 65536)
        except OSError: return False
        s = d.decode('utf-8', 'replace'); buf += s
        events.append([round(time.time() - t0, 4), 'o', s])
    return True
def idle(sec):
    end = time.time() + sec
    while time.time() < end: pump(0.02)
idle(0.8)
events.clear(); t0 = time.time(); buf = ''
# clear screen so recording starts clean
os.write(fd, b'clear\n'); idle(0.5); events.clear(); t0 = time.time(); buf = ''
os.write(fd, b''); idle(0.3)
events.append([0.0, 'o', '\x1b[H\x1b[2J'])
pump(0.1); events.append([round(time.time()-t0,4),'o','\x1b[1;34m~\x1b[0m \x1b[1;32m$\x1b[0m '])
first = True
for st in steps:
    k = st[0]
    if k == 'type':
        idle(0.5); buf = ''
        for ch in st[1]:
            os.write(fd, ch.encode()); idle(0.045)
        idle(0.35); os.write(fd, b'\r')
    elif k == 'key':
        os.write(fd, st[1].encode())
    elif k == 'sleep':
        idle(st[1])
    elif k == 'wait':
        mark = len(buf); to = st[2] if len(st) > 2 else 600; end = time.time() + to
        while not re.search(st[1], buf):
            if time.time() > end: print('timeout waiting for', st[1], file=sys.stderr); break
            pump(0.05)
    elif k == 'mark':
        buf = ''
# drop the first echo of 'clear' etc. Already cleared.
with open(out, 'w') as f:
    f.write(json.dumps({'version': 2, 'width': cols, 'height': rows, 'timestamp': int(t0), 'env': {'TERM': 'xterm-256color'}}) + '\n')
    for e in events: f.write(json.dumps(e) + '\n')
os.kill(pid, 9)
print('wrote', out, 'duration', events[-1][0] if events else 0)
