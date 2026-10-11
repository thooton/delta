# delta

This is the repository for the DELTA COMMAND CENTER and DELTA CLI.

The system is quite simple. There are three separate runtimes:

1. PHONE (Sonim XP3). The user has a Sonim XP3 flip phone. The Delta app (SOLD SEPARATELY) exists here and interfaces with the COMMAND CENTER.
2. COMMAND CENTER (VPS). The VPS runs the command center 24/7. The command center handles authentication. It also automatically updates from this GitHub repo. Its main purpose is to serve as a relay between the phone and anything else. The command center also handles authentication with and access to external services such as Canvas.
3. CLI (Agentic Software). What happens is kind of like this. Let's say that the user needs to fix one of the apps installed on their phone. So they open the Delta app, and they ask the agent hey, there is an issue with this app. So the apps are located at https://github.com/thooton/sonim which the agent has r/w access to (as well as this repo). So the agent will pull the repository and look through it and make changes, and push them. It will then use the delta CLI to push the updated APK to the command center. The user will see it in the files list on the app, and be able to decide whether to download it, delete it, after downloading, whether to install it, and whatever. Or, let's say that the user has a problem with delta itself, the command center, it will tell the agent, the agent has r/w access to https://github.com/thooton/delta, it will fix it, and the delta command center will auto-detect the change to the repo and rebuild and update itself.

BTW, the way that the agent knows that the CLI exists, is, the agent will NOT be told to clone this repo (this will just pollute its context), the agent will be told to download the delta CLI binary and run `delta help`, which will give it all the instructions it needs to understand what is going on and how to help the user.
