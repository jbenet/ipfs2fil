"""Makes the made-up demo content (photos, fractals) in the current folder."""
import math, random, os, colorsys
from PIL import Image, ImageDraw, ImageFilter
random.seed(7)
os.makedirs('photos/2024-iceland', exist_ok=True); os.makedirs('photos/2025-desert', exist_ok=True); os.makedirs('art', exist_ok=True)
W,H=1600,1067
def landscape(path, hue, seed):
    r=random.Random(seed); im=Image.new('RGB',(W,H)); d=ImageDraw.Draw(im)
    for y in range(H):
        t=y/H; c=colorsys.hls_to_rgb((hue+0.08*t)%1, 0.75-0.35*t, 0.55)
        d.line([(0,y),(W,y)], fill=tuple(int(255*v) for v in c))
    sx,sy=r.randint(200,1400), r.randint(120,380)
    d.ellipse([sx-70,sy-70,sx+70,sy+70], fill=(255,240,210))
    for layer in range(5):
        base=H*(0.45+0.11*layer); amp=60+r.random()*90; ph=r.random()*10
        pts=[(0,H)]
        for x in range(0,W+20,20):
            y=base+amp*math.sin(x/(170+layer*40)+ph)+amp*0.4*math.sin(x/53+ph*2)
            pts.append((x,y))
        pts.append((W,H))
        c=colorsys.hls_to_rgb((hue+0.5+layer*0.03)%1, 0.45-0.07*layer, 0.35)
        d.polygon(pts, fill=tuple(int(255*v) for v in c))
    im=im.filter(ImageFilter.GaussianBlur(1.2)); im.save(path, quality=90)
for i in range(14): landscape(f'photos/2024-iceland/IMG_{4100+i}.jpg', 0.55+0.02*i, i)
for i in range(10): landscape(f'photos/2025-desert/IMG_{5200+i}.jpg', 0.05+0.015*i, 100+i)
def mandel(path, cx, cy, sc, hue):
    w,h=1200,1200; im=Image.new('RGB',(w,h)); px=im.load()
    for y in range(h):
        for x in range(w):
            c=complex(cx+(x-w/2)*sc, cy+(y-h/2)*sc); z=0; n=0
            while abs(z)<4 and n<80: z=z*z+c; n+=1
            if n==80: px[x,y]=(10,10,20)
            else:
                rr,gg,bb=colorsys.hls_to_rgb((hue+n/80)%1,0.5,0.8); px[x,y]=(int(rr*255),int(gg*255),int(bb*255))
    im.save(path)
mandel('art/mandelbrot.png',-0.6,0,0.0025,0.6)
mandel('art/seahorse-valley.png',-0.745,0.11,0.00004,0.0)
mandel('art/elephants.png',0.28,0.008,0.00003,0.3)
