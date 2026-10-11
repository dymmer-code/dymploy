# Logo

Same visual family as the other Dymmer marks: the blue of Dymmer (`#5F82AD`), a rounded diamond on a
black squircle, and the symbol cut out of the diamond in the colour of the background. The symbol is a rocket,
pointing up and to the right, as in the 🚀 emoji: what is built, launched.

- `logo.svg`, `logo-1024.png`: the application icon (black squircle, transparent corners).
- `mark.svg`, `mark-1024.png`: the diamond alone, transparent, with the rocket as a real hole (the window is
  blue again): it works on any background.
- `avatar.svg`, `app-logo-512.png`: full-bleed black square, for the avatar of the GitHub App (GitHub asks for
  a PNG, JPG or GIF and shows it in a small square).

`logo.py` generates all of them from the geometry (a 500 px square rotated 45 degrees with a corner radius of 82,
as in the other Dymmer marks): `python3 logo.py` (the PNGs need ImageMagick).
